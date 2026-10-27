package collect

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

func TestRunValidatesConfig(t *testing.T) {
	cache, _ := store.Load(filepath.Join(t.TempDir(), "c.json"))
	for name, r := range map[string]*Runner{
		"no source": {Cache: cache, Client: wb.NewSupplierClient(1)},
		"no cache":  {Source: wb.IDList{1}, Client: wb.NewSupplierClient(1)},
		"no client": {Source: wb.IDList{1}, Cache: cache},
	} {
		if res, err := r.Run(context.Background()); err == nil || res != nil {
			t.Errorf("%s: res=%v err=%v, ожидалась ошибка конфигурации", name, res, err)
		}
	}
	// Без Client, но только поиск — допустимо.
	r := &Runner{Source: wb.IDList{1}, Cache: cache, SearchOnly: true}
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRunFetchesAndWritesCSV(t *testing.T) {
	cdn := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/vol0/data/supplier-by-id/1.json":
			_, _ = w.Write([]byte(`{"supplierName":"ООО Ромашка","inn":"7700000000","ogrn":"1027700000000","legalAddress":"Москва"}`))
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

func TestRunCountsFailedSuppliers(t *testing.T) {
	r, cache := newRunner(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden) // не повторяется
	}, wb.IDList{1, 2})
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("ошибки отдельных продавцов не должны ронять прогон: %v", err)
	}
	if res.Failed != 2 || cache.Len() != 0 {
		t.Fatalf("Failed = %d, cache = %d, want 2 и пустой кэш", res.Failed, cache.Len())
	}
}

// skippingSource ведёт себя как QuerySearch, у которого часть запросов упала.
type skippingSource struct{}

func (skippingSource) Collect(_ context.Context, emit func(wb.SellerHit)) error {
	emit(wb.SellerHit{SupplierID: 1})
	return fmt.Errorf("%w (1 из 2): %q: статус 403", wb.ErrQueriesSkipped, "куртка")
}

func TestRunSkippedQueriesBecomeWarning(t *testing.T) {
	r, _ := newRunner(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}, skippingSource{})
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("пропущенные запросы — не ошибка прогона: %v", err)
	}
	if len(res.IDs) != 1 || !strings.Contains(res.Warning, "1 из 2") {
		t.Fatalf("IDs=%v Warning=%q", res.IDs, res.Warning)
	}
}

func TestStaleNotFoundIsRefetched(t *testing.T) {
	var calls atomic.Int32
	r, cache := newRunner(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"supplierName":"появился"}`))
	}, wb.IDList{1, 2})
	cache.Put(&wb.Supplier{ID: 1, NotFound: true, FetchedAt: time.Now().Add(-48 * time.Hour)}) // отлежал
	cache.Put(&wb.Supplier{ID: 2, NotFound: true, FetchedAt: time.Now()})                      // свежий
	r.NotFoundTTL = 24 * time.Hour

	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("перепроверить нужно только отлежавший NotFound, CDN вызван %d раз", calls.Load())
	}
	if s, _ := cache.Get(1); s.NotFound || s.Name != "появился" {
		t.Fatalf("id 1 должен обновиться: %+v", s)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	cdn := func(_ http.ResponseWriter, r *http.Request) {
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
	r, _ := newRunner(t, func(_ http.ResponseWriter, _ *http.Request) {
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
	cdn := func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"supplierName":"x"}`))
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

// pageSource отдаёт всю «страницу» одним махом, не глядя на ctx между emit —
// так ведёт себя Searcher. Лишние id после лимита Runner должен отбросить.
type pageSource []int

func (p pageSource) Collect(ctx context.Context, emit func(wb.SellerHit)) error {
	for _, id := range p {
		emit(wb.SellerHit{SupplierID: id})
	}
	return ctx.Err()
}

func TestRunLimitIgnoresOverflowFromPage(t *testing.T) {
	r, _ := newRunner(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}, pageSource{1, 2, 3, 4, 5, 6, 7})
	r.Limit = 3
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.IDs) != 3 {
		t.Fatalf("IDs = %v, want ровно 3", res.IDs)
	}
}

type fakeEnricher struct {
	mu    sync.Mutex
	calls []string
	delay time.Duration
	// reply строит ответ; nil — обычное обогащение «ООО <ИНН>».
	reply func(inn string) (*wb.Enrichment, error)
}

func (f *fakeEnricher) Enrich(_ context.Context, inn string) (*wb.Enrichment, error) {
	f.mu.Lock()
	f.calls = append(f.calls, inn)
	f.mu.Unlock()
	time.Sleep(f.delay)
	if f.reply != nil {
		return f.reply(inn)
	}
	return &wb.Enrichment{Kind: "ЮЛ", ShortName: "ООО " + inn, Source: "test"}, nil
}

func (f *fakeEnricher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// cdnWithINN отдаёт продавцам ИНН по id: 1 — юрлицо (10 цифр), 2 — ИП (12 цифр),
// 3 — белорусский продавец без российского ИНН.
func cdnWithINN(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/vol0/data/supplier-by-id/1.json":
		_, _ = w.Write([]byte(`{"supplierName":"ЮЛ","inn":"7700000001"}`))
	case "/vol0/data/supplier-by-id/2.json":
		_, _ = w.Write([]byte(`{"supplierName":"ИП","inn":"770000000002"}`))
	case "/vol0/data/supplier-by-id/3.json":
		_, _ = w.Write([]byte(`{"supplierName":"BY","inn":"","unp":"123456789"}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestEnrichSkipsIPByDefault(t *testing.T) {
	r, cache := newRunner(t, cdnWithINN, wb.IDList{1, 2, 3})
	fe := &fakeEnricher{}
	r.Enricher = fe
	r.EnrichIP = false // ИП не трогаем

	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fe.calls) != 1 || fe.calls[0] != "7700000001" {
		t.Fatalf("обогащать должны только юрлицо, вызовы: %v", fe.calls)
	}
	s1, _ := cache.Get(1)
	s2, _ := cache.Get(2)
	s3, _ := cache.Get(3)
	if s1.Enrich == nil || s1.Enrich.ShortName != "ООО 7700000001" {
		t.Fatalf("юрлицо должно быть обогащено: %+v", s1.Enrich)
	}
	if s2.Enrich != nil {
		t.Fatalf("ИП не должен обогащаться без EnrichIP: %+v", s2.Enrich)
	}
	if s3.Enrich != nil {
		t.Fatalf("продавец без российского ИНН не обогащается: %+v", s3.Enrich)
	}
}

func TestEnrichIPAndOnRecord(t *testing.T) {
	r, _ := newRunner(t, cdnWithINN, wb.IDList{1, 2})
	r.Enricher = &fakeEnricher{}
	r.EnrichIP = true

	var mu sync.Mutex
	got := map[int]bool{}
	r.OnRecord = func(rec Record) error {
		mu.Lock()
		got[rec.Supplier.ID] = rec.Supplier.Enrich != nil
		mu.Unlock()
		return nil
	}
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[1] || !got[2] {
		t.Fatalf("оба продавца должны пройти через OnRecord обогащёнными: %v", got)
	}
}

// TestEnrichCachedSupplierDoesNotMutateCache: продавцы уже в кэше, обогащение
// дописывается во втором прогоне. Save каждые 1 запись читает объекты кэша,
// пока воркеры их обогащают, — под -race это ловит запись в объект на месте.
func TestEnrichCachedSupplierDoesNotMutateCache(t *testing.T) {
	const n = 12
	ids := make(wb.IDList, n)
	for i := range ids {
		ids[i] = i + 1
	}
	r, cache := newRunner(t, func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("всё в кэше, CDN не должен вызываться: %s", r.URL.Path)
	}, ids)
	for i := 1; i <= n; i++ {
		cache.Put(&wb.Supplier{ID: i, INN: fmt.Sprintf("77%08d", i)})
	}
	before, _ := cache.Get(1)
	r.Enricher = &fakeEnricher{delay: 2 * time.Millisecond}
	r.SaveEvery = 1
	r.Workers = 4

	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if before.Enrich != nil {
		t.Fatal("объект из кэша изменён на месте, а должен быть заменён копией")
	}
	after, _ := cache.Get(1)
	if after.Enrich == nil || after == before {
		t.Fatalf("в кэше должна лежать обогащённая копия: %+v", after)
	}
}

func TestEnrichNotFoundIsCached(t *testing.T) {
	r, cache := newRunner(t, cdnWithINN, wb.IDList{1})
	fe := &fakeEnricher{reply: func(string) (*wb.Enrichment, error) {
		return &wb.Enrichment{Source: "test", NotFound: true}, nil
	}}
	r.Enricher = fe

	for range 2 {
		if _, err := r.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if fe.count() != 1 {
		t.Fatalf("реестр спросили %d раз, а «не найдено» должно кэшироваться", fe.count())
	}
	if s, _ := cache.Get(1); s.Enrich == nil || !s.Enrich.NotFound {
		t.Fatalf("в кэше должна быть пометка NotFound: %+v", s.Enrich)
	}
}

func TestEnrichFailureIsCountedAndRetriedNextRun(t *testing.T) {
	r, cache := newRunner(t, cdnWithINN, wb.IDList{1})
	fe := &fakeEnricher{reply: func(string) (*wb.Enrichment, error) { return nil, errors.New("api down") }}
	r.Enricher = fe

	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("сбой обогащения не роняет прогон: %v", err)
	}
	if res.EnrichFailed != 1 || res.Failed != 0 {
		t.Fatalf("EnrichFailed=%d Failed=%d, want 1 и 0", res.EnrichFailed, res.Failed)
	}
	if s, ok := cache.Get(1); !ok || s.Enrich != nil {
		t.Fatalf("юрданные сохраняются, обогащение остаётся пустым: ok=%v %+v", ok, s)
	}
	// Второй прогон повторяет попытку, потому что в кэше нет пометки.
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fe.count() != 2 {
		t.Fatalf("calls = %d, want 2", fe.count())
	}
}

func TestEnrichUnavailableStopsRun(t *testing.T) {
	var cdnCalls atomic.Int32
	ids := make(wb.IDList, 200)
	for i := range ids {
		ids[i] = i + 1
	}
	r, cache := newRunner(t, func(w http.ResponseWriter, _ *http.Request) {
		cdnCalls.Add(1)
		_, _ = w.Write([]byte(`{"inn":"7700000001"}`))
	}, ids)
	errKey := fmt.Errorf("ключ отклонён: %w", wb.ErrEnrichUnavailable)
	fe := &fakeEnricher{reply: func(string) (*wb.Enrichment, error) { return nil, errKey }}
	r.Enricher = fe

	res, err := r.Run(context.Background())
	if !errors.Is(err, wb.ErrEnrichUnavailable) {
		t.Fatalf("err = %v, want ErrEnrichUnavailable", err)
	}
	if n := cdnCalls.Load(); n >= 200 {
		t.Fatalf("после отклонённого ключа прогон должен остановиться, CDN вызван %d раз", n)
	}
	if res == nil || len(res.IDs) != 200 {
		t.Fatal("частичный результат должен сохраниться")
	}
	// Юрданные продавца, на котором споткнулось обогащение, в кэше есть.
	if cache.Len() == 0 {
		t.Fatal("полученные юрданные должны остаться в кэше")
	}
}

func TestOnRecordErrorStopsRun(t *testing.T) {
	var cdnCalls atomic.Int32
	ids := make(wb.IDList, 200)
	for i := range ids {
		ids[i] = i + 1
	}
	r, _ := newRunner(t, func(w http.ResponseWriter, _ *http.Request) {
		cdnCalls.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}, ids)
	errPipe := errors.New("broken pipe")
	r.OnRecord = func(Record) error { return errPipe }

	_, err := r.Run(context.Background())
	if !errors.Is(err, errPipe) {
		t.Fatalf("err = %v, want errPipe", err)
	}
	if n := cdnCalls.Load(); n >= 200 {
		t.Fatalf("после ошибки вывода прогон должен остановиться, а CDN вызван %d раз", n)
	}
}
