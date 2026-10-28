package wbsearch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OkaneKatsuro/go-wb/internal/wb"
)

var (
	_ wb.Source        = Source{}
	_ wb.QuerySearcher = (*Runner)(nil)
)

// fakeBin кладёт в temp-каталог shell-скрипт, изображающий wb-search:
// разбирает -q/-pages/-out и выполняет body.
func fakeBin(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "wb-search")
	script := `#!/bin/sh
while [ $# -gt 0 ]; do case "$1" in -q) q="$2"; shift 2;; -pages) pages="$2"; shift 2;; -out) out="$2"; shift 2;; *) shift;; esac; done
` + body + "\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCollectReadsCSV(t *testing.T) {
	bin := fakeBin(t, `echo "токен получен"
printf '\357\273\277query,page,supplier,supplier_id\n%s,1,"ИП Иванов, И.",184\n%s,1,ООО Ромашка,7\n%s,2,,0\n' "$q" "$q" "$q" > "$out"
echo "страница 1: 2 товара (pages=$pages)"`)
	r := &Runner{Bin: bin, Dir: t.TempDir()}
	var hits []wb.SellerHit
	err := r.Collect(context.Background(), "куртка", 3, func(h wb.SellerHit) { hits = append(hits, h) })
	if err != nil {
		t.Fatal(err)
	}
	want := []wb.SellerHit{
		{SupplierID: 184, Supplier: "ИП Иванов, И.", Query: "куртка"},
		{SupplierID: 7, Supplier: "ООО Ромашка", Query: "куртка"},
	}
	if len(hits) != 2 || hits[0] != want[0] || hits[1] != want[1] {
		t.Fatalf("hits = %+v", hits)
	}
	if left, _ := filepath.Glob(filepath.Join(r.Dir, "wb-search-*.csv")); len(left) != 0 {
		t.Fatalf("временный CSV должен удаляться: %v", left)
	}
}

func TestCollectReportsFailure(t *testing.T) {
	r := &Runner{Bin: fakeBin(t, `echo "ничего не найдено" >&2; exit 1`), Dir: t.TempDir()}
	err := r.Collect(context.Background(), "q", 1, func(wb.SellerHit) {})
	if err == nil || !strings.Contains(err.Error(), "wb-search") {
		t.Fatalf("err = %v", err)
	}
}

func TestCollectStopsOnCancel(t *testing.T) {
	// Без exec sleep остался бы внуком; группа процессов накрывает и его.
	r := &Runner{Bin: fakeBin(t, `sleep 30`), Dir: t.TempDir()}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := r.Collect(ctx, "q", 1, func(wb.SellerHit) {})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("подпроцесс не остановлен по контексту: %s", time.Since(start))
	}
}

func TestSourceSkipsFailedQuery(t *testing.T) {
	bin := fakeBin(t, `if [ "$q" = "bad" ]; then exit 1; fi
printf 'supplier_id\n5\n' > "$out"`)
	src := Source{Runner: Runner{Bin: bin, Dir: t.TempDir()}, Queries: []string{"bad", "good"}, Pages: 1}
	var hits []wb.SellerHit
	err := src.Collect(context.Background(), func(h wb.SellerHit) { hits = append(hits, h) })
	if !errors.Is(err, wb.ErrQueriesSkipped) || len(hits) != 1 || hits[0].Query != "good" {
		t.Fatalf("err=%v hits=%+v", err, hits)
	}
}

func TestMissingBinary(t *testing.T) {
	r := &Runner{Bin: filepath.Join(t.TempDir(), "nope")}
	if err := r.Collect(context.Background(), "q", 1, func(wb.SellerHit) {}); err == nil {
		t.Fatal("ожидалась ошибка про отсутствующий бинарник")
	}
}
