// Пакет wb — предметная область Wildberries: продавец, источник продавцов,
// клиент публичного CDN с юридическими данными.
package wb

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// ErrQueriesSkipped — часть поисковых запросов не выполнилась (например, 403
// или 429 после повторов), остальные обработаны. Это предупреждение, а не
// отказ: Runner кладёт его в Result.Warning, чтобы «готово, 0 продавцов» не
// выглядело как успех, а Multi продолжает со следующим источником.

// SellerHit — один продавец, найденный источником Source. Обязателен только SupplierID.
type SellerHit struct {
	SupplierID     int
	Supplier       string
	SupplierRating float64
	Query          string // запрос, которым нашли продавца; пусто для списков id
}

// Source выдаёт найденных продавцов.
//
// Реализации в этом пакете: IDList (готовые id), QuerySearch (поиск WB по
// запросам), Multi (несколько источников подряд). Подойдёт что угодно, что умеет
// вызывать emit, поэтому новые способы находить продавцов подключаются сюда.
//
// Контракт: emit вызывается синхронно из той же горутины, что вызвала Collect,
// а Collect возвращает ctx.Err(), как только ctx отменён.
type Source interface {
	Collect(ctx context.Context, emit func(SellerHit)) error
}

// IDList — это Source поверх готового списка supplierId.
type IDList []int

func (l IDList) Collect(ctx context.Context, emit func(SellerHit)) error {
	for _, id := range l {
		if err := ctx.Err(); err != nil {
			return err
		}
		if id > 0 {
			emit(SellerHit{SupplierID: id})
		}
	}
	return nil
}

var ErrQueriesSkipped = errors.New("часть запросов пропущена")

// Multi запускает несколько источников по очереди и останавливается на первой
// ошибке. ErrQueriesSkipped не останавливает: он копится и возвращается в конце.
type Multi []Source

func (m Multi) Collect(ctx context.Context, emit func(SellerHit)) error {
	var warn error
	for _, s := range m {
		err := s.Collect(ctx, emit)
		switch {
		case err == nil:
		case errors.Is(err, ErrQueriesSkipped):
			warn = errors.Join(warn, err)
		default:
			return err
		}
	}
	return warn
}

// QuerySearcher умеет пройти страницы одного запроса и отдать продавцов.
// Реализация — wbsearch.Runner (cmd/wb-search подпроцессом).
type QuerySearcher interface {
	Collect(ctx context.Context, query string, maxPages int, emit func(SellerHit)) error
}

// QuerySearch прогоняет Searcher по нескольким запросам.
//
// Упавший запрос логируется и пропускается, а в конце Collect возвращает
// ErrQueriesSkipped со сводкой.
type QuerySearch struct {
	Searcher QuerySearcher
	Queries  []string
	Pages    int
}

func (q QuerySearch) Collect(ctx context.Context, emit func(SellerHit)) error {
	var skipped []string
	for i, query := range q.Queries {
		slog.Info("поиск", "n", i+1, "of", len(q.Queries), "query", query)
		err := q.Searcher.Collect(ctx, query, q.Pages, emit)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			slog.Warn("запрос пропущен", "query", query, "err", err)
			skipped = append(skipped, fmt.Sprintf("%q: %v", query, err))
		}
	}
	if len(skipped) == 0 {
		return nil
	}
	total := len(skipped)
	const show = 3 // в сводке хватит первых причин, остальные есть в логе
	more := ""
	if total > show {
		more = fmt.Sprintf(" и ещё %d", total-show)
		skipped = skipped[:show]
	}
	return fmt.Errorf("%w (%d из %d): %s%s", ErrQueriesSkipped, total, len(q.Queries), strings.Join(skipped, "; "), more)
}

// Sleep ждёт d или отмены ctx — что наступит раньше.
// time.Sleep прервать нельзя, а это можно. Используется и клиентами других
// пакетов (ofdata), чтобы пауза между повторами везде была одинаковой.
func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
