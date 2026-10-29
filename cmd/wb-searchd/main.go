// wb-searchd — HTTP-обёртка над поиском по витрине для pipeline (лидген CRM):
//
//	GET /search?q=<запрос>&pages=<1..30>  → {"query":…,"pages":N,"products":[…]}
//	GET /healthz                          → ok
//
// Товары отдаются в полях витрины WB (supplierId, feedbacks, totalQuantity…),
// поэтому pipeline сворачивает их в продавцов тем же кодом, что и прямую
// выдачу. Запросы выполняются по одному: на каждый запускается свой браузер и
// закрывается по окончании, остальные ждут в очереди. Ключей сервис не держит
// и наружу не публикуется: к нему ходит только pipeline по сети Docker.
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
	"syscall"
	"time"

	"github.com/OkaneKatsuro/go-wb/internal/storefront"
)

func main() {
	addr := flag.String("addr", ":8090", "адрес HTTP")
	timeout := flag.Duration("timeout", 3*time.Minute, "предел на один поиск: браузер, сессия, страницы")
	maxPages := flag.Int("max-pages", 30, "максимум страниц в одном запросе")
	flag.Parse()

	search := func(ctx context.Context, query string, pages int) ([]storefront.Item, error) {
		return storefront.Search(ctx, query, pages, func(format string, args ...any) {
			slog.Info("wb-search", "query", query, "msg", fmt.Sprintf(format, args...))
		})
	}
	srv := &http.Server{
		Addr:              *addr,
		Handler:           newServer(search, *timeout, *maxPages).routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// WriteTimeout не ставим: поиск длится минуты, ответ уходит в конце.
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	slog.Info("wb-searchd", "addr", *addr, "timeout", timeout.String(), "max_pages", *maxPages)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}
}
