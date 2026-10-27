package collect

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OkaneKatsuro/go-wb/internal/store"
	"github.com/OkaneKatsuro/go-wb/internal/wb"
)

func newRunner(t *testing.T, cdn http.HandlerFunc, src wb.Source) (*Runner, *store.Cache) {
	t.Helper()
	srv := httptest.NewServer(cdn)
	t.Cleanup(srv.Close)
	client := wb.NewSupplierClient(1000)
	client.BaseURL = srv.URL
	client.Backoff = time.Millisecond
	cache, err := store.Load(filepath.Join(t.TempDir(), "cache.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &Runner{Source: src, Client: client, Cache: cache, Workers: 3}, cache
}

func TestRunFetchesAndWritesCSV(t *testing.T) {
	cdn := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/vol0/data/supplier-by-id/1.json":
			w.Write([]byte(`{"supplierName":"ООО Ромашка","inn":"7700000000","ogrn":"1027700000000","legalAddress":"Москва"}`))
		case "/vol0/data/supplier-by-id/2.json":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}
	// Дубликаты и неотсортированный вход — намеренно.
	r, cache := newRunner(t, cdn, wb.IDList{2, 1, 1})
	var last Progress
	r.OnProgress = func(p Progress) { last = p }

	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := res.IDs; len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("IDs = %v, want [1 2]", got)
	}
	if last.Stage != StageDone {
		t.Fatalf("last progress stage = %s, want done", last.Stage)
	}
	if s, ok := cache.Get(2); !ok || !s.NotFound {
		t.Fatalf("id 2 should be cached as NotFound, got %+v ok=%v", s, ok)
	}

	var buf bytes.Buffer
	n, err := WriteCSV(&buf, res, cache)
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if n != 2 || !strings.HasPrefix(out, "\xEF\xBB\xBF") {
		t.Fatalf("n=%d, BOM=%v", n, strings.HasPrefix(out, "\xEF\xBB\xBF"))
	}
	if !strings.Contains(out, "1,ООО Ромашка,,7700000000,1027700000000,,Москва") {
		t.Fatalf("row for id 1 is wrong:\n%s", out)
	}
	if !strings.Contains(out, "\n2,,,,,,,,,,false,true,") {
		t.Fatalf("row for id 2 should be a NotFound row:\n%s", out)
	}

	// Второй прогон: всё в кэше, до CDN дойти не должно.
	r.Client.BaseURL = "http://127.0.0.1:1" // здесь никто не слушает
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("cached run should not touch the network: %v", err)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	cdn := func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // никогда не отвечает; выходит, когда клиент отключается
	}
	ids := make(wb.IDList, 50)
	for i := range ids {
		ids[i] = i + 1
	}
	r, _ := newRunner(t, cdn, ids)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	res, err := r.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("cancel took %s, workers did not stop", time.Since(start))
	}
	if len(res.IDs) != 50 {
		t.Fatalf("partial result must keep the ids, got %d", len(res.IDs))
	}
}

func TestRunSearchOnly(t *testing.T) {
	r, _ := newRunner(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("CDN must not be called in search-only mode")
	}, wb.IDList{3, 1})
	r.SearchOnly = true
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.IDs) != 2 || res.IDs[0] != 1 {
		t.Fatalf("IDs = %v", res.IDs)
	}
}

// endlessSource шлёт продавцов с растущими id, пока ctx не отменят.
// Так проверяется, что Runner.Limit останавливает сбор, а не ждёт конца источника.
type endlessSource struct{ emitted int }

func (s *endlessSource) Collect(ctx context.Context, emit func(wb.SellerHit)) error {
	for id := 1; ; id++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		emit(wb.SellerHit{SupplierID: id})
		s.emitted++
	}
}

func TestRunStopsAtLimit(t *testing.T) {
	cdn := func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"supplierName":"x"}`))
	}
	src := &endlessSource{}
	r, _ := newRunner(t, cdn, src)
	r.Limit = 5

	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("достижение лимита не должно быть ошибкой: %v", err)
	}
	if len(res.IDs) != 5 {
		t.Fatalf("собрано %d продавцов, ожидалось 5", len(res.IDs))
	}
}
