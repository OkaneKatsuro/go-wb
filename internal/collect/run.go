// Пакет collect — это конвейер: найти продавцов через Source, затем получить
// юрданные по каждому из них ограниченным пулом воркеров.
package collect

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"

	"github.com/OkaneKatsuro/go-wb/internal/store"
	"github.com/OkaneKatsuro/go-wb/internal/wb"
)

type Stage string

const (
	StageSearch Stage = "search"
	StageLegal  Stage = "legal"
	StageDone   Stage = "done"
)

// Progress сообщается через Runner.OnProgress. Total равен 0, пока неизвестен.
type Progress struct {
	Stage   Stage
	Message string
	Done    int
	Total   int
}

// Result — то, что дал прогон. IDs отсортированы; в Hits по записи на каждый id.
type Result struct {
	IDs     []int
	Hits    map[int]wb.SellerHit
	Failed  int    // запросы юрданных, сдавшиеся после повторов
	Warning string // нефатальная проблема, напр. поиск остановлен ErrChallenge
}

// Enricher обогащает продавца по ИНН данными из внешнего реестра.
// Возвращает (nil, nil), если запись не найдена. Реализация — ofdata.Client.
type Enricher interface {
	Enrich(ctx context.Context, inn string) (*wb.Enrichment, error)
}

// Record — готовые данные по одному продавцу. OnRecord вызывается по мере
// завершения каждого продавца, поэтому порядок — порядок завершения, не отсортированный.
type Record struct {
	Hit      wb.SellerHit
	Supplier *wb.Supplier // юрданные с CDN плюс Supplier.Enrich, если включено обогащение
}

type Runner struct {
	Source  wb.Source
	Client  *wb.SupplierClient
	Cache   *store.Cache
	Workers int // одновременные запросы к CDN; клиент ограничивает их скорость глобально

	SearchOnly bool     // остановиться после этапа Source
	SaveEvery  int      // сохранять кэш каждые N полученных продавцов, 0 = 200
	Limit      int      // остановить сбор, набрав столько уникальных продавцов, 0 = без лимита
	Enricher   Enricher // nil = без обогащения
	EnrichIP   bool     // обогащать ли ИП (ИНН из 12 цифр); для юрлиц обогащение всегда

	// OnProgress необязателен. Всегда вызывается из той горутины, что вызвала Run.
	OnProgress func(Progress)
	// OnRecord необязателен. Вызывается из воркеров по мере готовности продавца,
	// поэтому должен быть потокобезопасным. Для потокового NDJSON-вывода.
	OnRecord func(Record)
}

// wantEnrich сообщает, нужно ли обогащать этого продавца и можно ли (по ИНН).
func (r *Runner) wantEnrich(s *wb.Supplier) bool {
	return r.Enricher != nil && s != nil && !s.NotFound && s.INN != "" &&
		(!wb.IsIP(s.INN) || r.EnrichIP)
}

func (r *Runner) report(p Progress) {
	if r.OnProgress != nil {
		r.OnProgress(p)
	}
}

// Run выполняет оба этапа. При ошибке возвращённый Result всё равно содержит
// всё, что успели собрать, чтобы вызывающий мог сохранить частичный результат.
func (r *Runner) Run(ctx context.Context) (*Result, error) {
	res := &Result{Hits: map[int]wb.SellerHit{}}

	// Отдельный контекст сбора: достигнув лимита, отменяем только его, чтобы
	// следующий этап (юрданные) работал на исходном ctx.
	collectCtx, stopCollect := context.WithCancel(ctx)
	defer stopCollect()
	reachedLimit := false

	r.report(Progress{Stage: StageSearch, Message: "сбор supplierId"})
	err := r.Source.Collect(collectCtx, func(h wb.SellerHit) {
		if _, ok := res.Hits[h.SupplierID]; ok {
			return
		}
		res.Hits[h.SupplierID] = h
		r.report(Progress{Stage: StageSearch, Message: "сбор supplierId", Done: len(res.Hits), Total: r.Limit})
		if r.Limit > 0 && len(res.Hits) >= r.Limit {
			reachedLimit = true
			stopCollect() // цель набрана, источник остановится на ближайшей проверке ctx
		}
	})
	res.IDs = slices.Sorted(maps.Keys(res.Hits))
	switch {
	case err == nil:
	case reachedLimit && ctx.Err() == nil:
		// Сбор прервали мы сами, набрав лимит. Это штатное завершение, не ошибка.
	case errors.Is(err, wb.ErrChallenge):
		res.Warning = err.Error()
		slog.Warn("поиск остановлен, продолжаю с тем, что собрано", "sellers", len(res.IDs))
	default:
		return res, err
	}
	if r.SearchOnly {
		r.report(Progress{Stage: StageDone, Message: "готово", Done: len(res.IDs), Total: len(res.IDs)})
		return res, nil
	}

	// Работа нужна, если нет юрданных в кэше, либо данные есть, но продавца
	// ещё не обогащали, а обогащение включено и применимо. В режиме потока
	// (OnRecord задан) через воркеры проходят все продавцы, чтобы каждый был
	// отдан ровно один раз; уже готовые обрабатываются мгновенно, без сети.
	streaming := r.OnRecord != nil
	var todo []int
	for _, id := range res.IDs {
		s, ok := r.Cache.Get(id)
		need := !ok || (s.Enrich == nil && r.wantEnrich(s))
		if need || streaming {
			todo = append(todo, id)
		}
	}
	slog.Info("юрданные", "total", len(res.IDs), "обработать", len(todo))

	ferr := r.fetchAll(ctx, todo, res)
	// Сохраняем даже после отмены: всё полученное к этому моменту стоит сберечь.
	if err := r.Cache.Save(); err != nil {
		slog.Warn("сохранение кэша", "err", err)
		if ferr == nil {
			ferr = fmt.Errorf("сохранение кэша: %w", err)
		}
	}
	if ferr != nil {
		return res, ferr
	}
	r.report(Progress{Stage: StageDone, Message: "готово", Done: len(res.IDs), Total: len(res.IDs)})
	return res, nil
}

// fetchAll — пул воркеров по схеме fan-out/fan-in.
//
//	производитель ──jobs──▶ N воркеров ──results──▶ эта горутина
//
// Производитель перестаёт подавать задачи при отмене, воркеры дочитывают канал
// и выходят, а канал results закрывается последним, поэтому цикл ниже всегда завершается.
func (r *Runner) fetchAll(ctx context.Context, todo []int, res *Result) error {
	if len(todo) == 0 {
		return ctx.Err()
	}
	workers := max(r.Workers, 1)
	saveEvery := r.SaveEvery
	if saveEvery <= 0 {
		saveEvery = 200
	}

	jobs := make(chan int)
	results := make(chan error)

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for id := range jobs {
				results <- r.process(ctx, id, res)
			}
		})
	}
	go func() {
	feed:
		for _, id := range todo {
			select {
			case jobs <- id:
			case <-ctx.Done():
				break feed
			}
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	done := 0
	for err := range results {
		done++
		if err != nil && ctx.Err() == nil {
			res.Failed++
			slog.Warn("юрданные", "err", err)
		}
		if done%saveEvery == 0 {
			if err := r.Cache.Save(); err != nil {
				slog.Warn("кэш", "err", err)
			}
		}
		r.report(Progress{Stage: StageLegal, Message: "юрданные", Done: done, Total: len(todo)})
	}
	return ctx.Err()
}

// process обрабатывает одного продавца: берёт юрданные из кэша или с CDN,
// при необходимости обогащает через реестр, сохраняет в кэш и отдаёт в OnRecord.
// Запускается из воркеров, поэтому пишет только в потокобезопасные Cache/OnRecord.
func (r *Runner) process(ctx context.Context, id int, res *Result) error {
	s, ok := r.Cache.Get(id)
	if !ok {
		fetched, err := r.Client.Get(ctx, id)
		if err != nil {
			return err
		}
		s = fetched
	}

	if s.Enrich == nil && r.wantEnrich(s) {
		e, err := r.Enricher.Enrich(ctx, s.INN)
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			slog.Warn("обогащение", "inn", s.INN, "err", err)
		} else if e != nil {
			s.Enrich = e
		}
	}

	r.Cache.Put(s)
	if r.OnRecord != nil {
		r.OnRecord(Record{Hit: res.Hits[id], Supplier: s})
	}
	return nil
}
