package wb

import (
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// newTestSearcher строит Searcher на реальном профиле site-mimic, но направляет
// его в httptest-сервер и убирает все паузы.
func newTestSearcher(t *testing.T, h http.HandlerFunc) *Searcher {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	s, err := NewSearcher("../../windows-chrome152.json", "", "-1257786", "v18")
	if err != nil {
		t.Fatal(err)
	}
	s.baseURL = srv.URL
	s.minDelay, s.maxDelay, s.retryWait, s.longWait = 0, 0, 0, 0
	return s
}

func collectHits(t *testing.T, s *Searcher, query string, pages int) ([]SellerHit, error) {
	t.Helper()
	var hits []SellerHit
	err := s.Collect(context.Background(), query, pages, func(h SellerHit) { hits = append(hits, h) })
	return hits, err
}

func TestSearcherPaginatesUntilEmptyPage(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	s := newTestSearcher(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.URL.Query().Get("query") != "куртка" {
			t.Errorf("query = %q", r.URL.Query().Get("query"))
		}
		switch r.URL.Query().Get("page") {
		case "1":
			_, _ = w.Write([]byte(`{"products":[{"id":1,"supplier":"A","supplierId":10,"supplierRating":4.5},{"id":2,"supplierId":0}]}`))
		case "2":
			_, _ = w.Write([]byte(`{"data":{"products":[{"id":3,"supplier":"B","supplierId":20}]}}`)) // старая форма ответа
		default:
			_, _ = w.Write([]byte(`{"products":[]}`))
		}
	})
	hits, err := collectHits(t, s, "куртка", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].SupplierID != 10 || hits[0].Query != "куртка" || hits[1].SupplierID != 20 {
		t.Fatalf("hits = %+v", hits)
	}
	if len(paths) != 3 || !strings.HasPrefix(paths[0], "/exactmatch/ru/common/v18/search") {
		t.Fatalf("paths = %v, want 3 запроса к шарду common", paths)
	}
}

func TestSearcherDecodesGzip(t *testing.T) {
	s := newTestSearcher(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, _ = gz.Write([]byte(`{"products":[{"id":1,"supplierId":7}]}`))
		_ = gz.Close()
	})
	hits, err := collectHits(t, s, "q", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].SupplierID != 7 {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestSearcherChallengeStops(t *testing.T) {
	s := newTestSearcher(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(statusChallenge)
	})
	if _, err := collectHits(t, s, "q", 3); !errors.Is(err, ErrChallenge) {
		t.Fatalf("err = %v, want ErrChallenge", err)
	}
}

func TestSearcherRotatesShardOn429(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	s := newTestSearcher(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		n := len(paths)
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"products":[]}`))
	})
	if _, err := collectHits(t, s, "q", 1); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || !strings.Contains(paths[0], "/common/") || !strings.Contains(paths[1], "/male/") {
		t.Fatalf("после 429 шард должен смениться: %v", paths)
	}
}

func TestSearcherGivesUpAfter429Retries(t *testing.T) {
	calls := 0
	s := newTestSearcher(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := collectHits(t, s, "q", 1)
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("err = %v", err)
	}
	if calls != 3 { // попытка + 2 повтора
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestSearcherHonoursCancel(t *testing.T) {
	s := newTestSearcher(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"products":[{"id":1,"supplierId":1}]}`))
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s.Collect(ctx, "q", 100, func(SellerHit) {})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestQuerySearchSkipsFailedQueryButStopsOnChallenge(t *testing.T) {
	s := newTestSearcher(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("query") {
		case "bad":
			w.WriteHeader(http.StatusBadRequest) // не повторяется, запрос пропускается
		case "challenge":
			w.WriteHeader(statusChallenge)
		default:
			_, _ = w.Write([]byte(`{"products":[{"id":1,"supplierId":5}]}`))
		}
	})
	var hits []SellerHit
	emit := func(h SellerHit) { hits = append(hits, h) }

	q := QuerySearch{Searcher: s, Queries: []string{"bad", "good"}, Pages: 1}
	if err := q.Collect(context.Background(), emit); err != nil {
		t.Fatalf("упавший запрос должен пропускаться: %v", err)
	}
	if len(hits) != 1 || hits[0].Query != "good" {
		t.Fatalf("hits = %+v", hits)
	}

	hits = nil
	q = QuerySearch{Searcher: s, Queries: []string{"challenge", "good"}, Pages: 1}
	if err := q.Collect(context.Background(), emit); !errors.Is(err, ErrChallenge) {
		t.Fatalf("err = %v, want ErrChallenge", err)
	}
	if len(hits) != 0 {
		t.Fatalf("после 498 следующие запросы не должны выполняться: %+v", hits)
	}
}

func TestMultiAndIDList(t *testing.T) {
	var hits []SellerHit
	src := Multi{IDList{3, 0, -1, 4}, IDList{5}}
	if err := src.Collect(context.Background(), func(h SellerHit) { hits = append(hits, h) }); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 || hits[0].SupplierID != 3 || hits[2].SupplierID != 5 {
		t.Fatalf("hits = %+v", hits)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := src.Collect(ctx, func(SellerHit) {}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
