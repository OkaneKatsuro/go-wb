package web

import (
	"encoding/json"
	"io"
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
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func post(t *testing.T, url string, form url.Values) (int, string) {
	t.Helper()
	resp, err := http.PostForm(url, form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
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
			w.Write([]byte(`{"supplierName":"ООО Ромашка","inn":"7700000000"}`))
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
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("кросс-доменный старт: %d, ожидался 403", resp.StatusCode)
	}

	// Без Origin (как curl) — проходит.
	if code, _ := post(t, ts.URL+"/api/start", url.Values{"mode": {"ids"}, "input": {"1"}}); code != 202 {
		t.Fatalf("старт без Origin: %d, ожидался 202", code)
	}
}
