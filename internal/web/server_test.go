package web

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OkaneKatsuro/go-wb/internal/store"
	"github.com/OkaneKatsuro/go-wb/internal/wb"
)

func newTestServer(t *testing.T, cdn http.HandlerFunc) (*httptest.Server, string) {
	t.Helper()
	cdnSrv := httptest.NewServer(cdn)
	t.Cleanup(cdnSrv.Close)
	client := wb.NewSupplierClient(1000)
	client.BaseURL = cdnSrv.URL
	client.Backoff = time.Millisecond

	dir := t.TempDir()
	cache, err := store.Load(filepath.Join(dir, "cache.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{DataDir: dir, Cache: cache, Client: client, Workers: 2})
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	t.Cleanup(s.Stop) // выполнится первым: Cleanup работает по LIFO
	return ts, dir
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func post(t *testing.T, url string, form url.Values) (int, string) {
	t.Helper()
	resp, err := http.PostForm(url, form)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func waitFinished(t *testing.T, base string) State {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, body := get(t, base+"/api/state")
		var st State
		if err := json.Unmarshal([]byte(body), &st); err != nil {
			t.Fatal(err)
		}
		if st.ID != 0 && !st.Running {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("job did not finish in time")
	return State{}
}

func TestIndexHealthAndEmptyState(t *testing.T) {
	ts, _ := newTestServer(t, http.NotFound)

	code, body := get(t, ts.URL+"/")
	if code != 200 || !strings.Contains(body, "WB Sellers") {
		t.Fatalf("index: %d %.100s", code, body)
	}
	if strings.Contains(body, "Поисковые запросы") {
		t.Fatal("query mode must be hidden when NewSearch is nil")
	}
	if code, body := get(t, ts.URL+"/healthz"); code != 200 || body != "ok" {
		t.Fatalf("healthz: %d %q", code, body)
	}
	if code, _ := get(t, ts.URL+"/download"); code != 404 {
		t.Fatalf("download without result: %d, want 404", code)
	}
	if code, _ := get(t, ts.URL+"/nope"); code != 404 {
		t.Fatalf("unknown path: %d, want 404", code)
	}
}

func TestFullFlow(t *testing.T) {
	ts, dir := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/vol0/data/supplier-by-id/1.json":
			_, _ = w.Write([]byte(`{"supplierName":"ООО Ромашка","inn":"7700000000"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	if code, _ := post(t, ts.URL+"/api/start", url.Values{"mode": {"ids"}, "input": {"# только комментарий\n"}}); code != 400 {
		t.Fatalf("empty input: %d, want 400", code)
	}
	if code, _ := post(t, ts.URL+"/api/start", url.Values{"mode": {"queries"}, "input": {"x"}}); code != 400 {
		t.Fatalf("query mode disabled: %d, want 400", code)
	}

	code, body := post(t, ts.URL+"/api/start", url.Values{
		"mode":  {"ids"},
		"input": {"1\nhttps://www.wildberries.ru/seller/2?sort=popular\n"},
	})
	if code != 202 {
		t.Fatalf("start: %d %s", code, body)
	}

	st := waitFinished(t, ts.URL)
	if st.Error != "" || st.Stage != "done" || st.Sellers != 2 || st.Rows != 2 {
		t.Fatalf("final state: %+v", st)
	}

	code, csv := get(t, ts.URL+"/download")
	if code != 200 || !strings.Contains(csv, "ООО Ромашка") || strings.Count(csv, "\n") != 3 {
		t.Fatalf("download: %d\n%s", code, csv)
	}
	if _, err := os.Stat(filepath.Join(dir, "sellers-1.csv")); err != nil {
		t.Fatalf("csv file not written: %v", err)
	}
}

func TestBusyAndCancel(t *testing.T) {
	ts, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // висим, пока не отменят
	})
	ids := strings.Repeat("1\n2\n3\n4\n5\n", 1)
	if code, _ := post(t, ts.URL+"/api/start", url.Values{"mode": {"ids"}, "input": {ids}}); code != 202 {
		t.Fatalf("start: %d", code)
	}
	if code, _ := post(t, ts.URL+"/api/start", url.Values{"mode": {"ids"}, "input": {ids}}); code != 409 {
		t.Fatalf("second start while running: %d, want 409", code)
	}
	if code, _ := post(t, ts.URL+"/api/cancel", nil); code != 202 {
		t.Fatalf("cancel: %d, want 202", code)
	}
	st := waitFinished(t, ts.URL)
	if st.Stage != "cancelled" || st.Running {
		t.Fatalf("after cancel: %+v", st)
	}
	if code, _ := post(t, ts.URL+"/api/cancel", nil); code != 409 {
		t.Fatalf("cancel with nothing running: %d, want 409", code)
	}
}

func TestCrossOriginRejected(t *testing.T) {
	ts, _ := newTestServer(t, http.NotFound)

	// Origin с чужого домена — сервер должен ответить 403.
	req, _ := http.NewRequest("POST", ts.URL+"/api/start", strings.NewReader("mode=ids&input=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("кросс-доменный старт: %d, ожидался 403", resp.StatusCode)
	}

	// Без Origin (как curl) — проходит.
	if code, _ := post(t, ts.URL+"/api/start", url.Values{"mode": {"ids"}, "input": {"1"}}); code != 202 {
		t.Fatalf("старт без Origin: %d, ожидался 202", code)
	}
}

func TestForeignHostRejected(t *testing.T) {
	ts, _ := newTestServer(t, http.NotFound)

	// Чужой Host (как при DNS-rebinding) — 403, даже на безобидном GET.
	req, _ := http.NewRequest("GET", ts.URL+"/healthz", nil)
	req.Host = "attacker.example"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("чужой Host: %d, ожидался 403", resp.StatusCode)
	}

	// Loopback (127.0.0.1 из ts.URL) проходит — обычный healthz.
	if code, body := get(t, ts.URL+"/healthz"); code != 200 || body != "ok" {
		t.Fatalf("loopback healthz: %d %q", code, body)
	}
}

func TestAllowedHostPasses(t *testing.T) {
	cache, _ := store.Load(t.TempDir() + "/c.json")
	srv := New(Config{
		DataDir:      t.TempDir(),
		Cache:        cache,
		Client:       wb.NewSupplierClient(1000),
		Workers:      1,
		AllowedHosts: []string{"sellers.local"},
	})
	t.Cleanup(srv.Stop)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest("GET", ts.URL+"/healthz", nil)
	req.Host = "sellers.local"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("разрешённый Host: %d, ожидался 200", resp.StatusCode)
	}
}

// newTestServerWithSearch включает режим поиска: NewSearch отдаёт IDList из
// «запросов»-чисел, а pages запоминает, чтобы проверить разбор поля формы.
func newTestServerWithSearch(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	cache, _ := store.Load(filepath.Join(t.TempDir(), "c.json"))
	gotPages := new(int)
	s := New(Config{
		DataDir: t.TempDir(), Cache: cache, Client: wb.NewSupplierClient(1000), Workers: 1,
		NewSearch: func(queries []string, pages int) (wb.Source, error) {
			*gotPages = pages
			return wb.IDList(wb.ParseIDs(queries)), nil
		},
	})
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	t.Cleanup(s.Stop)
	return ts, gotPages
}

func TestNumericFieldsValidated(t *testing.T) {
	ts, gotPages := newTestServerWithSearch(t)
	for _, form := range []url.Values{
		{"mode": {"queries"}, "input": {"1"}, "pages": {"abc"}},
		{"mode": {"queries"}, "input": {"1"}, "pages": {"0"}},
		{"mode": {"queries"}, "input": {"1"}, "pages": {"51"}},
		{"mode": {"ids"}, "input": {"1"}, "limit": {"-1"}},
		{"mode": {"ids"}, "input": {"1"}, "limit": {"1e3"}},
	} {
		if code, body := post(t, ts.URL+"/api/start", form); code != 400 {
			t.Errorf("%v: %d %s, want 400", form, code, body)
		}
	}
	// В режиме ids скрытое поле pages отправляется браузером, но не мешает.
	if code, body := post(t, ts.URL+"/api/start", url.Values{"mode": {"ids"}, "input": {"1"}, "pages": {"abc"}}); code != 202 {
		t.Fatalf("pages в режиме ids: %d %s", code, body)
	}
	waitFinished(t, ts.URL)
	// Пустые поля — значения по умолчанию.
	if code, body := post(t, ts.URL+"/api/start", url.Values{"mode": {"queries"}, "input": {"1"}, "pages": {""}, "limit": {""}}); code != 202 {
		t.Fatalf("пустые поля: %d %s", code, body)
	}
	waitFinished(t, ts.URL)
	if *gotPages != defaultPages {
		t.Fatalf("pages = %d, want %d по умолчанию", *gotPages, defaultPages)
	}
}

// TestEventsSurviveReadTimeout пинит свойство net/http, на которое опирается
// cmd/wb-server: ReadTimeout действует только пока читается запрос и не рвёт
// долгий ответ. ReadTimeout здесь 100 мс, поток читается 500 мс.
func TestEventsSurviveReadTimeout(t *testing.T) {
	cache, _ := store.Load(filepath.Join(t.TempDir(), "c.json"))
	s := New(Config{DataDir: t.TempDir(), Cache: cache, Client: wb.NewSupplierClient(1), Workers: 1})
	t.Cleanup(s.Stop)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: s, ReadTimeout: 100 * time.Millisecond}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	resp, err := http.Get("http://" + ln.Addr().String() + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	closed := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(resp.Body)
		closed <- err
	}()
	select {
	case err := <-closed:
		t.Fatalf("SSE-поток закрылся раньше времени: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestTooLongLineRejected(t *testing.T) {
	ts, _ := newTestServer(t, http.NotFound)
	code, body := post(t, ts.URL+"/api/start", url.Values{"mode": {"ids"}, "input": {strings.Repeat("1", 2<<20)}})
	if code != 400 || !strings.Contains(body, "ввод не разобран") {
		t.Fatalf("строка в 2 МБ: %d %s", code, body)
	}
}

func TestSecurityHeaders(t *testing.T) {
	ts, _ := newTestServer(t, http.NotFound)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	for h, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := resp.Header.Get(h); got != want {
			t.Errorf("%s = %q, want %q", h, got, want)
		}
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP = %q", csp)
	}
}

func TestEventsStreamSendsState(t *testing.T) {
	ts, _ := newTestServer(t, http.NotFound)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var st State
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &st); err != nil {
		t.Fatalf("первое событие не State: %q: %v", line, err)
	}
	if st.ID != 0 || st.Running {
		t.Fatalf("до первого запуска State пустой, got %+v", st)
	}
}

func TestDownloadAfterCancelKeepsPartialResult(t *testing.T) {
	ts, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/vol0/data/supplier-by-id/1.json" {
			_, _ = w.Write([]byte(`{"supplierName":"Быстрый"}`))
			return
		}
		<-r.Context().Done() // остальные висят до отмены
	})
	if code, _ := post(t, ts.URL+"/api/start", url.Values{"mode": {"ids"}, "input": {"1\n2\n3\n"}}); code != 202 {
		t.Fatalf("start: %d", code)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, body := get(t, ts.URL+"/api/state")
		if strings.Contains(body, `"done":1`) || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code, _ := post(t, ts.URL+"/api/cancel", nil); code != 202 {
		t.Fatalf("cancel: %d", code)
	}
	st := waitFinished(t, ts.URL)
	if st.Stage != "cancelled" || st.Sellers != 3 {
		t.Fatalf("после отмены: %+v", st)
	}
	code, csv := get(t, ts.URL+"/download")
	if code != 200 || !strings.Contains(csv, "Быстрый") || strings.Count(csv, "\n") != 4 {
		t.Fatalf("частичный CSV после отмены: %d\n%s", code, csv)
	}
}

func TestStartAcceptsSearchCSVWithLimit(t *testing.T) {
	ts, _ := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"supplierName":"x"}`))
	})
	csvIn := "\xEF\xBB\xBFquery,id,name,supplier_id\n" +
		"куртка,1,\"a, b\",101\nкуртка,2,c,102\nкуртка,3,d,101\nкуртка,4,e,103\n"
	code, body := post(t, ts.URL+"/api/start", url.Values{"mode": {"ids"}, "input": {csvIn}, "limit": {"2"}})
	if code != 202 {
		t.Fatalf("start: %d %s", code, body)
	}
	st := waitFinished(t, ts.URL)
	if st.Stage != "done" || st.Sellers != 2 || st.Rows != 2 {
		t.Fatalf("CSV с лимитом 2: %+v", st)
	}
}
