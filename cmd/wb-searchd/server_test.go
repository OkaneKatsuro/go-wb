package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OkaneKatsuro/go-wb/internal/storefront"
)

func fakeSearch(items []storefront.Item, err error) searchFunc {
	return func(context.Context, string, int) ([]storefront.Item, error) { return items, err }
}

func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestSearchRejectsBadParams(t *testing.T) {
	h := newServer(fakeSearch(nil, nil), time.Second, 30).routes()
	for _, target := range []string{
		"/search",
		"/search?q=",
		"/search?q=" + strings.Repeat("я", 201),
		"/search?q=чай&pages=0",
		"/search?q=чай&pages=31",
		"/search?q=чай&pages=abc",
	} {
		if rec := get(t, h, target); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: код %d, ожидался 400", target, rec.Code)
		}
	}
}

func TestSearchReturnsStorefrontProducts(t *testing.T) {
	items := []storefront.Item{{ID: 1, SupplierID: 7, Supplier: "ООО Ромашка", Feedbacks: 12, Quantity: 3}}
	h := newServer(fakeSearch(items, nil), time.Second, 30).routes()

	rec := get(t, h, "/search?q=чай%20оптом&pages=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body)
	}
	var got struct {
		Query    string           `json:"query"`
		Pages    int              `json:"pages"`
		Products []map[string]any `json:"products"`
		Warning  string           `json:"warning"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Query != "чай оптом" || got.Pages != 2 || len(got.Products) != 1 || got.Warning != "" {
		t.Fatalf("ответ: %+v", got)
	}
	p := got.Products[0]
	if p["supplierId"] != float64(7) || p["feedbacks"] != float64(12) || p["totalQuantity"] != float64(3) {
		t.Errorf("поля витрины WB: %v", p)
	}
}

func TestSearchEmptyResultIsArrayNotNull(t *testing.T) {
	h := newServer(fakeSearch(nil, nil), time.Second, 30).routes()
	rec := get(t, h, "/search?q=чай")
	if !strings.Contains(rec.Body.String(), `"products":[]`) {
		t.Errorf("ожидался пустой массив: %s", rec.Body)
	}
}

func TestSearchFailureWithoutItemsIs502(t *testing.T) {
	h := newServer(fakeSearch(nil, errors.New("кука x_wbaas_token не появилась")), time.Second, 30).routes()
	rec := get(t, h, "/search?q=чай")
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "x_wbaas_token") {
		t.Errorf("код %d, тело %q", rec.Code, rec.Body)
	}
}

func TestSearchPartialResultCarriesWarning(t *testing.T) {
	items := []storefront.Item{{ID: 1}}
	h := newServer(fakeSearch(items, errors.New("страница 2: http 429")), time.Second, 30).routes()
	rec := get(t, h, "/search?q=чай&pages=3")
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d", rec.Code)
	}
	var got response
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Products) != 1 || !strings.Contains(got.Warning, "429") {
		t.Errorf("ответ: %+v", got)
	}
}

// Один браузер за раз: параллельные запросы не должны накладываться.
func TestSearchRunsOneAtATime(t *testing.T) {
	var running, maxRunning int32
	search := func(context.Context, string, int) ([]storefront.Item, error) {
		n := atomic.AddInt32(&running, 1)
		for {
			m := atomic.LoadInt32(&maxRunning)
			if n <= m || atomic.CompareAndSwapInt32(&maxRunning, m, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		atomic.AddInt32(&running, -1)
		return nil, nil
	}
	ts := httptest.NewServer(newServer(search, time.Second, 30).routes())
	defer ts.Close()

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(ts.URL + "/search?q=чай")
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
		}()
	}
	wg.Wait()
	if maxRunning != 1 {
		t.Errorf("одновременных поисков %d, ожидался 1", maxRunning)
	}
}

func TestHealthz(t *testing.T) {
	rec := get(t, newServer(fakeSearch(nil, nil), time.Second, 30).routes(), "/healthz")
	if rec.Code != http.StatusOK {
		t.Errorf("код %d", rec.Code)
	}
}
