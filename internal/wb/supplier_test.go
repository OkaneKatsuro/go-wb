package wb

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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
	// Каталог всегда vol0: формула vol{id/1000} давала 404 всем id от 1000.
	tests := map[int]string{
		1:         "https://cdn/vol0/data/supplier-by-id/1.json",
		999:       "https://cdn/vol0/data/supplier-by-id/999.json",
		123456:    "https://cdn/vol0/data/supplier-by-id/123456.json",
		250057880: "https://cdn/vol0/data/supplier-by-id/250057880.json",
	}
	for id, want := range tests {
		if got := c.url(id); got != want {
			t.Errorf("url(%d) = %s, want %s", id, got, want)
		}
	}
}

func TestValidINN(t *testing.T) {
	tests := map[string]bool{
		"7707083893":    true,  // юрлицо
		"544052334508":  true,  // ИП
		"":              false, // нет ИНН
		"770708389":     false, // 9 цифр
		"77070838931":   false, // 11 цифр
		"7707O83893":    false, // буква O вместо нуля
		"123456789":     false, // УНП Беларуси
		"123456789012x": false,
	}
	for inn, want := range tests {
		if got := ValidINN(inn); got != want {
			t.Errorf("ValidINN(%q) = %v, want %v", inn, got, want)
		}
	}
	if !IsIP("544052334508") || IsIP("7707083893") {
		t.Fatal("IsIP: 12 цифр — ИП, 10 — юрлицо")
	}
}

func TestGetRetriesOn429(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/vol0/data/supplier-by-id/123456.json" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"supplierName":"ООО Ромашка","inn":"7700000000","ogrn":"","ogrnip":"300000000000000"}`))
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
	if s.FetchedAt.IsZero() {
		t.Fatal("FetchedAt должен быть проставлен")
	}
}

func TestGetGivesUpAfterRetriesOn5xx(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	_, err := c.Get(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v, want статус 503", err)
	}
	if calls.Load() != 4 { // первая попытка + 3 повтора
		t.Fatalf("calls = %d, want 4", calls.Load())
	}
}

func TestGetNotFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	s, err := c.Get(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	if !s.NotFound || s.ID != 5 || s.FetchedAt.IsZero() {
		t.Fatalf("want NotFound stub for id 5 with FetchedAt, got %+v", s)
	}
}

func TestGetDoesNotRetryOn403(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
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

func TestGetRejectsHugeBody(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"supplierName":"` + strings.Repeat("x", maxSupplierBody) + `"}`))
	})
	_, err := c.Get(context.Background(), 1)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("err = %v, want ErrBodyTooLarge", err)
	}
}

func TestGetHonoursCancel(t *testing.T) {
	c := newTestClient(t, func(_ http.ResponseWriter, r *http.Request) {
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

func TestGetRateLimits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	c := NewSupplierClient(20) // 50 мс на запрос
	c.BaseURL = srv.URL

	start := time.Now()
	for i := range 5 {
		if _, err := c.Get(context.Background(), i+1); err != nil {
			t.Fatal(err)
		}
	}
	// Первый запрос мгновенный (burst 1), остальные четыре ждут по 50 мс.
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Fatalf("5 запросов при 20 rps заняли %s, лимит не работает", d)
	}
}
