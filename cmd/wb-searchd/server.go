package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/OkaneKatsuro/go-wb/internal/storefront"
)

// searchFunc — поиск по витрине; в тестах подменяется заглушкой.
type searchFunc func(ctx context.Context, query string, pages int) ([]storefront.Item, error)

const maxQueryRunes = 200

// server обслуживает /search и /healthz. Поиски идут по одному: на каждый
// поднимается свой браузер, параллельные запросы ждут в очереди slot.
type server struct {
	search   searchFunc
	slot     chan struct{}
	timeout  time.Duration
	maxPages int
}

func newServer(search searchFunc, timeout time.Duration, maxPages int) *server {
	return &server{search: search, slot: make(chan struct{}, 1), timeout: timeout, maxPages: maxPages}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /search", s.handleSearch)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux
}

// response — ответ /search. Products в полях витрины WB (см. storefront.Item).
type response struct {
	Query    string            `json:"query"`
	Pages    int               `json:"pages"`
	Products []storefront.Item `json:"products"`
	// Warning — поиск оборвался на одной из страниц; товары до неё отданы.
	Warning string `json:"warning,omitempty"`
}

func (s *server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" || len([]rune(q)) > maxQueryRunes {
		http.Error(w, fmt.Sprintf("q: нужен запрос до %d символов", maxQueryRunes), http.StatusBadRequest)
		return
	}
	pages := 1
	if v := r.URL.Query().Get("pages"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > s.maxPages {
			http.Error(w, fmt.Sprintf("pages: число от 1 до %d", s.maxPages), http.StatusBadRequest)
			return
		}
		pages = n
	}

	// Очередь на единственный браузер. Ждём вместе с контекстом запроса:
	// ушедший клиент не должен занимать место.
	select {
	case s.slot <- struct{}{}:
		defer func() { <-s.slot }()
	case <-r.Context().Done():
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()
	started := time.Now()
	items, err := s.search(ctx, q, pages)
	took := time.Since(started).Round(time.Millisecond)

	resp := response{Query: q, Pages: pages, Products: items}
	if resp.Products == nil {
		resp.Products = []storefront.Item{} // в JSON [] вместо null
	}
	if err != nil {
		if len(items) == 0 {
			slog.Error("search failed", "query", q, "pages", pages, "took", took.String(), "err", err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		resp.Warning = err.Error()
	}
	slog.Info("search", "query", q, "pages", pages, "products", len(resp.Products), "took", took.String(), "warning", resp.Warning)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Warn("write response", "query", q, "err", err)
	}
}
