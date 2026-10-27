package collect

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/OkaneKatsuro/go-wb/internal/wb"
)

type failWriter struct{ err error }

func (f failWriter) Write([]byte) (int, error) { return 0, f.err }

func TestNDJSONWriterWritesOneObjectPerLine(t *testing.T) {
	var buf bytes.Buffer
	w := NewNDJSONWriter(&buf)
	recs := []Record{
		{Hit: wb.SellerHit{SupplierID: 1, Supplier: "из поиска", Query: "q"}, Supplier: &wb.Supplier{ID: 1, INN: "7700000001"}},
		{Hit: wb.SellerHit{SupplierID: 2}, Supplier: &wb.Supplier{ID: 2, Name: "ООО", Enrich: &wb.Enrichment{Kind: "ЮЛ", Source: "t"}}},
		{Hit: wb.SellerHit{SupplierID: 3}}, // Supplier == nil не должен ронять
	}
	for _, r := range recs {
		if err := w.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	if w.Count() != 3 {
		t.Fatalf("Count = %d", w.Count())
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("строк %d:\n%s", len(lines), buf.String())
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if first["supplierName"] != "из поиска" || first["sourceQuery"] != "q" || first["inn"] != "7700000001" {
		t.Fatalf("first = %v", first)
	}
	if !strings.Contains(lines[1], `"enrich":{"kind":"ЮЛ"`) || !strings.Contains(lines[2], `"supplierId":3`) {
		t.Fatalf("lines = %q", lines)
	}
}

func TestNDJSONWriterReturnsWriteError(t *testing.T) {
	errPipe := errors.New("broken pipe")
	w := NewNDJSONWriter(failWriter{errPipe})
	err := w.Write(Record{Supplier: &wb.Supplier{ID: 1}})
	if !errors.Is(err, errPipe) {
		t.Fatalf("err = %v, want errPipe", err)
	}
	if w.Count() != 0 {
		t.Fatalf("неудачная запись не считается: Count = %d", w.Count())
	}
}
