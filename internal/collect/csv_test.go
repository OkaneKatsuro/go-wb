package collect

import (
	"bytes"
	"strings"
	"testing"

	"github.com/OkaneKatsuro/go-wb/internal/store"
	"github.com/OkaneKatsuro/go-wb/internal/wb"
)

func TestCsvSafe(t *testing.T) {
	tests := map[string]string{
		"":                "",
		"ООО Ромашка":     "ООО Ромашка",
		"=1+1":            "'=1+1",
		"+79990000000":    "'+79990000000",
		"-1,5":            "'-1,5",
		"@SUM(A1)":        "'@SUM(A1)",
		"\tтаб":           "'\tтаб",
		"обычный - дефис": "обычный - дефис", // дефис не в начале — не трогаем
	}
	for in, want := range tests {
		if got := csvSafe(in); got != want {
			t.Errorf("csvSafe(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWriteCSVEscapesFormula(t *testing.T) {
	cache, _ := store.Load(t.TempDir() + "/c.json")
	res := &Result{
		IDs:  []int{1},
		Hits: map[int]wb.SellerHit{1: {SupplierID: 1, Supplier: "=HYPERLINK(\"evil\")"}},
	}
	var buf bytes.Buffer
	if _, err := WriteCSV(&buf, res, cache); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"'=HYPERLINK(""evil"")"`) {
		t.Fatalf("формула не обезврежена:\n%s", buf.String())
	}
}
