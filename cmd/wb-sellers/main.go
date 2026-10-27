// wb-sellers собирает юридические данные продавцов Wildberries из командной строки.
//
// Шаг 1: найти supplierId через Source (файл с id, поиск WB по запросам).
// Шаг 2: получить юрданные по каждому supplierId из публичного CDN.
// Результат: CSV целиком в конце или NDJSON построчно по мере готовности.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/OkaneKatsuro/go-wb/internal/collect"
	"github.com/OkaneKatsuro/go-wb/internal/ofdata"
	"github.com/OkaneKatsuro/go-wb/internal/store"
	"github.com/OkaneKatsuro/go-wb/internal/wb"
)

// Коды выхода: 1 — ошибка, 2 — неверные флаги, 130 — остановлено сигналом (как в shell).
const (
	exitErr      = 1
	exitUsage    = 2
	exitCanceled = 130
)

// config хранит все флаги. flag.StringVar пишет прямо в поле,
// поэтому дальше программа работает с обычными значениями, а не с указателями.
type config struct {
	queries, ids, out, idsOut, profile, proxy, dest, version, cache, dump string
	format, ofdataKey                                                     string
	pages, rps, limit, ofdataRps                                          int
	searchOnly, enrich, enrichIP                                          bool
}

func main() {
	os.Exit(realMain())
}

// realMain возвращает код выхода, чтобы os.Exit вызывался один раз снаружи
// и все defer (в том числе остановка обработчика сигналов) успели отработать.
func realMain() int {
	var c config
	flag.StringVar(&c.queries, "queries", "", "файл с поисковыми запросами, один на строку")
	flag.StringVar(&c.ids, "ids", "", "файл с supplierId или ссылками на продавца, один на строку")
	flag.IntVar(&c.pages, "pages", 10, "максимум страниц на запрос (по 100 товаров)")
	flag.StringVar(&c.out, "out", "", "итоговый файл: out/sellers.csv или out/sellers.ndjson по формату; «-» = stdout")
	flag.StringVar(&c.idsOut, "ids-out", "out/supplier_ids.txt", "куда записать найденные supplierId (пусто = не писать)")
	flag.BoolVar(&c.searchOnly, "search-only", false, "только поиск, без юрданных")
	flag.StringVar(&c.profile, "profile", "windows-chrome152.json", "профиль site-mimic для поиска")
	flag.StringVar(&c.proxy, "proxy", "", "прокси для поиска, http://user:pass@host:port")
	flag.StringVar(&c.dest, "dest", "-1257786", "регион WB (dest)")
	flag.StringVar(&c.version, "api-version", "v18", "версия поискового API")
	flag.IntVar(&c.rps, "rps", 5, "запросов в секунду к CDN юрданных")
	flag.IntVar(&c.limit, "limit", 0, "остановить сбор, набрав столько уникальных продавцов (0 = без лимита)")
	flag.StringVar(&c.cache, "cache", "out/cache.json", "кэш юрданных")
	flag.StringVar(&c.dump, "dump", "out/first_response.json", "сохранить первый сырой ответ поиска (пусто = не сохранять)")
	flag.StringVar(&c.format, "format", "csv", "формат вывода: csv или ndjson")
	flag.BoolVar(&c.enrich, "enrich", false, "обогащать юрданные из реестра OFData")
	flag.BoolVar(&c.enrichIP, "enrich-ip", false, "обогащать и ИП (12-значный ИНН), иначе только юрлица")
	flag.StringVar(&c.ofdataKey, "ofdata-key", "", "ключ OFData (по умолчанию из переменной OFDATA_KEY)")
	flag.IntVar(&c.ofdataRps, "ofdata-rps", 3, "запросов в секунду к OFData")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if err := c.validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		flag.Usage()
		return exitUsage
	}

	// Ctrl+C или SIGTERM отменяют ctx; всё, что ниже, проверяет его и останавливается.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Без этого запись в закрытый пайп (`-out - | head`) убивает процесс SIGPIPE
	// раньше, чем NDJSONWriter вернёт ошибку и Runner сохранит кэш. С Ignore
	// запись возвращает EPIPE, и прогон завершается штатно: кэш сохранён, код 1.
	signal.Ignore(syscall.SIGPIPE)

	if err := run(ctx, c); err != nil {
		if errors.Is(err, context.Canceled) {
			slog.Warn("остановлено пользователем, собранное сохранено")
			return exitCanceled
		}
		slog.Error(err.Error())
		return exitErr
	}
	return 0
}

// validate проверяет флаги до любой работы, чтобы ошибка в них не всплыла
// после минуты поиска. Здесь же подставляется имя выходного файла по формату.
func (c *config) validate() error {
	if c.queries == "" && c.ids == "" {
		return errors.New("нужен -queries или -ids")
	}
	switch c.format {
	case "csv", "ndjson":
	default:
		return fmt.Errorf("неизвестный -format %q (csv или ndjson)", c.format)
	}
	if c.out == "" {
		c.out = "out/sellers." + c.format
	}
	if c.enrich && c.ofdataKey == "" {
		c.ofdataKey = os.Getenv("OFDATA_KEY")
	}
	if c.enrich && c.ofdataKey == "" {
		return errors.New("для -enrich нужен ключ OFData: переменная OFDATA_KEY или флаг -ofdata-key")
	}
	return nil
}

func run(ctx context.Context, c config) error {
	for _, p := range []string{c.out, c.cache, c.idsOut, c.dump} {
		if p != "" && p != "-" {
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
		}
	}

	// Источники идут по порядку. Список id не падает, поэтому он первый;
	// тогда поиск, остановленный ErrChallenge, не сможет потерять эти id.
	var sources wb.Multi
	if c.ids != "" {
		lines, err := wb.ReadLines(c.ids)
		if err != nil {
			return err
		}
		sources = append(sources, wb.IDList(wb.ParseIDs(lines)))
	}
	if c.queries != "" {
		queries, err := wb.ReadLines(c.queries)
		if err != nil {
			return err
		}
		s, err := wb.NewSearcher(c.profile, c.proxy, c.dest, c.version)
		if err != nil {
			return fmt.Errorf("site-mimic: %w", err)
		}
		s.DumpPath = c.dump
		sources = append(sources, wb.QuerySearch{Searcher: s, Queries: queries, Pages: c.pages})
	}

	cache, err := store.Load(c.cache)
	if err != nil {
		return err
	}
	runner := &collect.Runner{
		Source:     sources,
		Client:     wb.NewSupplierClient(c.rps),
		Cache:      cache,
		Workers:    c.rps * 2,
		SearchOnly: c.searchOnly,
		Limit:      c.limit,
		OnProgress: cliProgress(),
	}

	// Обогащение из OFData, если включено. Ключ проверен в validate.
	if c.enrich {
		en, err := ofdata.New(c.ofdataKey, c.ofdataRps)
		if err != nil {
			return err
		}
		runner.Enricher = en
		runner.EnrichIP = c.enrichIP
	}

	// NDJSON отдаётся потоком по мере готовности продавцов.
	var ndw *collect.NDJSONWriter
	closeOut := func() error { return nil }
	if c.format == "ndjson" && !c.searchOnly {
		w, closeFn, err := openOut(c.out)
		if err != nil {
			return err
		}
		closeOut = closeFn
		ndw = collect.NewNDJSONWriter(w)
		runner.OnRecord = ndw.Write
	}

	res, err := runner.Run(ctx)
	// Ошибка закрытия файла — это потерянные данные, её нельзя проглотить.
	if cerr := closeOut(); cerr != nil && err == nil {
		err = fmt.Errorf("закрытие %s: %w", c.out, cerr)
	}

	// Частичный результат стоит записать, даже когда err != nil.
	if res != nil && c.idsOut != "" && len(res.IDs) > 0 {
		if werr := collect.WriteIDs(c.idsOut, res); werr != nil {
			slog.Warn("ids-out", "err", werr)
		} else {
			slog.Info("supplierId записаны", "file", c.idsOut, "count", len(res.IDs))
		}
	}
	if err != nil {
		return err
	}
	if c.searchOnly {
		return nil
	}
	if c.format == "ndjson" {
		slog.Info("готово", "rows", ndw.Count(), "format", "ndjson", "failed", res.Failed, "enrichFailed", res.EnrichFailed)
		return nil
	}
	n, err := writeCSVOut(c.out, res, cache)
	if err != nil {
		return err
	}
	slog.Info("готово", "rows", n, "file", c.out, "failed", res.Failed, "enrichFailed", res.EnrichFailed)
	return nil
}

// openOut возвращает writer для вывода: stdout при "-", иначе файл.
// Для stdout закрывать нечего, поэтому closer — пустая функция.
func openOut(path string) (io.Writer, func() error, error) {
	if path == "-" {
		return os.Stdout, func() error { return nil }, nil
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, nil, err
	}
	return f, f.Close, nil
}

// writeCSVOut пишет CSV в файл или в stdout (при "-").
func writeCSVOut(path string, res *collect.Result, cache *store.Cache) (int, error) {
	if path == "-" {
		return collect.WriteCSV(os.Stdout, res, cache)
	}
	return collect.WriteCSVFile(path, res, cache)
}

// cliProgress печатает смену этапа и каждую сотую запись юрданных.
func cliProgress() func(collect.Progress) {
	var last collect.Stage
	return func(p collect.Progress) {
		switch {
		case p.Stage != last:
			last = p.Stage
			slog.Info(p.Message, "stage", p.Stage, "done", p.Done, "total", p.Total)
		case p.Stage == collect.StageLegal && p.Done%100 == 0:
			slog.Info("юрданные", "done", p.Done, "total", p.Total)
		}
	}
}
