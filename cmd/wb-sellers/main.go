// wb-sellers собирает юридические данные продавцов Wildberries из командной строки.
//
// Шаг 1: найти supplierId через Source (файл с id, поиск WB по запросам).
// Шаг 2: получить юрданные по каждому supplierId из публичного CDN.
// Результат: CSV.
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

// config хранит все флаги. flag.StringVar пишет прямо в поле,
// поэтому дальше программа работает с обычными значениями, а не с указателями.
type config struct {
	queries, ids, out, idsOut, profile, proxy, dest, version, cache, dump string
	format, ofdataKey                                                     string
	pages, rps, limit, ofdataRps                                          int
	searchOnly, enrich, enrichIP                                          bool
}

func main() {
	var c config
	flag.StringVar(&c.queries, "queries", "", "файл с поисковыми запросами, один на строку")
	flag.StringVar(&c.ids, "ids", "", "файл с supplierId или ссылками на продавца, один на строку")
	flag.IntVar(&c.pages, "pages", 10, "максимум страниц на запрос (по 100 товаров)")
	flag.StringVar(&c.out, "out", "out/sellers.csv", "итоговый CSV")
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

	if c.queries == "" && c.ids == "" {
		fmt.Fprintln(os.Stderr, "нужен -queries или -ids")
		flag.Usage()
		os.Exit(2)
	}

	// Ctrl+C или SIGTERM отменяют ctx; всё, что ниже, проверяет его и останавливается.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, c); err != nil {
		if errors.Is(err, context.Canceled) {
			slog.Warn("остановлено пользователем, собранное сохранено")
			os.Exit(130)
		}
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, c config) error {
	for _, p := range []string{c.out, c.cache, c.idsOut, c.dump} {
		if p != "" {
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

	if c.format != "csv" && c.format != "ndjson" {
		return fmt.Errorf("неизвестный -format %q (csv или ndjson)", c.format)
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

	// Обогащение из OFData, если включено.
	if c.enrich {
		key := c.ofdataKey
		if key == "" {
			key = os.Getenv("OFDATA_KEY")
		}
		en, err := ofdata.New(key, c.ofdataRps)
		if err != nil {
			return err
		}
		runner.Enricher = en
		runner.EnrichIP = c.enrichIP
	}

	// NDJSON отдаётся потоком по мере готовности продавцов.
	var ndw *collect.NDJSONWriter
	var closeOut func() error
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
	if closeOut != nil {
		closeOut()
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
		slog.Info("готово", "rows", ndw.Count(), "format", "ndjson", "failed", res.Failed)
		return nil
	}
	n, err := writeCSVOut(c.out, res, cache)
	if err != nil {
		return err
	}
	slog.Info("готово", "rows", n, "file", c.out, "failed", res.Failed)
	return nil
}

// openOut возвращает writer для вывода: stdout при "-" или пустом пути, иначе файл.
func openOut(path string) (io.Writer, func() error, error) {
	if path == "" || path == "-" {
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
	if path == "" || path == "-" {
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
