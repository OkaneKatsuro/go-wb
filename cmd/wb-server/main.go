// wb-server — браузерный интерфейс для wb-sellers: вставить id, нажать запуск,
// смотреть прогресс, скачать CSV.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/OkaneKatsuro/go-wb/internal/store"
	"github.com/OkaneKatsuro/go-wb/internal/wb"
	"github.com/OkaneKatsuro/go-wb/internal/web"
)

type config struct {
	addr, data, profile, proxy, dest, version string
	rps                                       int
}

func main() {
	var c config
	flag.StringVar(&c.addr, "addr", "127.0.0.1:8080", "адрес HTTP-сервера (слушать все интерфейсы: :8080)")
	flag.StringVar(&c.data, "data", "out", "каталог для кэша и CSV")
	flag.StringVar(&c.profile, "profile", "windows-chrome152.json", "профиль site-mimic для режима поиска (пусто = режим выключен)")
	flag.StringVar(&c.proxy, "proxy", "", "прокси для поиска")
	flag.StringVar(&c.dest, "dest", "-1257786", "регион WB (dest)")
	flag.StringVar(&c.version, "api-version", "v18", "версия поискового API")
	flag.IntVar(&c.rps, "rps", 5, "запросов в секунду к CDN юрданных")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if err := run(c); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func run(c config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(c.data, 0o755); err != nil {
		return err
	}
	cache, err := store.Load(filepath.Join(c.data, "cache.json"))
	if err != nil {
		return err
	}

	cfg := web.Config{
		DataDir: c.data,
		Cache:   cache,
		Client:  wb.NewSupplierClient(c.rps),
		Workers: c.rps * 2,
	}
	switch _, err := os.Stat(c.profile); {
	case c.profile == "":
		slog.Info("режим поиска выключен")
	case err != nil:
		slog.Warn("профиль не найден, режим поиска выключен", "profile", c.profile)
	default:
		// Это замыкание и есть шов: верни здесь любой другой wb.Source,
		// чтобы поменять, как находятся продавцы в режиме поиска.
		cfg.NewSearch = func(queries []string, pages int) (wb.Source, error) {
			s, err := wb.NewSearcher(c.profile, c.proxy, c.dest, c.version)
			if err != nil {
				return nil, fmt.Errorf("site-mimic: %w", err)
			}
			return wb.QuerySearch{Searcher: s, Queries: queries, Pages: pages}, nil
		}
	}
	app := web.New(cfg)
	srv := &http.Server{Addr: c.addr, Handler: app, ReadHeaderTimeout: 10 * time.Second}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("сервер запущен", "addr", c.addr, "data", c.data)

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		slog.Info("получен сигнал, останавливаюсь")
	}

	// Порядок важен: завершить задачу (она сохраняет кэш), освободить SSE-потоки,
	// затем перестать принимать соединения, затем сохранить кэш ещё раз.
	app.Stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := cache.Save(); err != nil {
		return fmt.Errorf("кэш: %w", err)
	}
	slog.Info("остановлен")
	return nil
}
