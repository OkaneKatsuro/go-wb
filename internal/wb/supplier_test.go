package wb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient направляет SupplierClient на фейковый CDN и убирает все ожидания.
func newTestClient(t *testing.T, h http.HandlerFunc) *SupplierClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := NewSupplierClient(1000)
	c.BaseURL = srv.URL
	c.Backoff = time.Millisecond
	return c
}

func TestSupplierURL(t *testing.T) {
	c := &SupplierClient{BaseURL: "https://cdn"}
	tests := map[int]string{
		1:      "https://cdn/vol0/data/supplier-by-id/1.json",
		999:    "https://cdn/vol0/data/supplier-by-id/999.json",
		123456: "https://cdn/vol123/data/supplier-by-id/123456.json",
	}
	for id, want := range tests {
		if got := c.url(id); got != want {
			t.Errorf("url(%d) = %s, want %s", id, got, want)
		}
	}
}

func TestGetRetriesOn429(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/vol123/data/supplier-by-id/123456.json" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"supplierName":"ООО Ромашка","inn":"7700000000","ogrn":"","ogrnip":"300000000000000"}`))
	})
	s, err := c.Get(context.Background(), 123456)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
	if s.ID != 123456 || s.Name != "ООО Ромашка" || s.INN != "7700000000" {
		t.Fatalf("unexpected supplier %+v", s)
	}
	if s.OGRN != "300000000000000" {
		t.Fatalf("OGRN should fall back to OGRNIP, got %q", s.OGRN)
	}
}

func TestGetNotFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	s, err := c.Get(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	if !s.NotFound || s.ID != 5 {
		t.Fatalf("want NotFound stub for id 5, got %+v", s)
	}
}

func TestGetDoesNotRetryOn403(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
	})
	if _, err := c.Get(context.Background(), 1); err == nil {
		t.Fatal("expected error")
	}
	if calls.Load() != 1 {
		t.Fatalf("403 must not be retried, calls = %d", calls.Load())
	}
}

func TestGetHonoursCancel(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // висим, пока клиент не сдастся
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Get(ctx, 1)
	if err == nil {
		t.Fatal("expected error")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("Get ignored context, took %s", time.Since(start))
	}
}
