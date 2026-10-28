package collect

import (
	"bytes"
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OkaneKatsuro/go-wb/internal/store"
	"github.com/OkaneKatsuro/go-wb/internal/wb"
)

func TestCSVSafe(t *testing.T) {
	tests := map[string]string{
		"":                "",
		"ООО Ромашка":     "ООО Ромашка",
		"=1+1":            "'=1+1",
		"+79990000000":    "'+79990000000",
		"-1,5":            "'-1,5", // не число (запятая) — экранируем
		"-20":             "-20",   // отрицательное число — не формула
		"-1.5":            "-1.5",
		"-.5":             "-.5",
		"-inf":            "'-inf", // ParseFloat такое принял бы, Excel — нет
		"-Infinity":       "'-Infinity",
		"-NaN":            "'-NaN",
		"-0x1p-2":         "'-0x1p-2",
		"-1e3":            "'-1e3",
		"-1.2.3":          "'-1.2.3",
		"-":               "'-",
		"-SUM(A1)":        "'-SUM(A1)",
		"@SUM(A1)":        "'@SUM(A1)",
		"\tтаб":           "'\tтаб",
		"обычный - дефис": "обычный - дефис", // дефис не в начале — не трогаем
	}
	for in, want := range tests {
		if got := CSVSafe(in); got != want {
			t.Errorf("CSVSafe(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExcelWriterSanitizesAndKeepsRow(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewExcelWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	row := []string{"=cmd", "ok"}
	if err := w.Write(row); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if row[0] != "=cmd" {
		t.Fatal("Write не должен менять переданный слайс")
	}
	if got := buf.String(); got != "\xEF\xBB\xBF'=cmd,ok\n" {
		t.Fatalf("got %q", got)
	}
}

func TestCSVRowWidthMatchesHeader(t *testing.T) {
	cache, _ := store.Load(filepath.Join(t.TempDir(), "c.json"))
	cache.Put(&wb.Supplier{ID: 1, Name: "A", Enrich: &wb.Enrichment{Kind: "ЮЛ", Director: "Иванов", Phones: []string{"+7", "+8"}}})
	cache.Put(&wb.Supplier{ID: 2, Name: "B", Enrich: &wb.Enrichment{NotFound: true}})
	// id 3 в кэше нет вовсе
	for _, id := range []int{1, 2, 3} {
		if got := len(csvRow(id, wb.SellerHit{SupplierID: id}, cache)); got != len(csvHeader) {
			t.Errorf("id %d: ширина строки %d, заголовка %d", id, got, len(csvHeader))
		}
	}
	row := csvRow(2, wb.SellerHit{}, cache)
	if row[len(baseHeader)] != "" {
		t.Fatalf("при NotFound колонки обогащения пустые, got %q", row[len(baseHeader):])
	}
	row = csvRow(1, wb.SellerHit{}, cache)
	if got := row[len(baseHeader)+10]; got != "+7; +8" {
		t.Fatalf("телефоны через «; »: %q", got)
	}
}

func TestWriteCSVEscapesFormula(t *testing.T) {
	cache, _ := store.Load(filepath.Join(t.TempDir(), "c.json"))
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

func TestWriteCSVFileIsAtomicAndParsable(t *testing.T) {
	dir := t.TempDir()
	cache, _ := store.Load(filepath.Join(dir, "c.json"))
	cache.Put(&wb.Supplier{ID: 7, Name: "ООО \"Кавычки\", и запятая", INN: "7700000007"})
	res := &Result{IDs: []int{7}, Hits: map[int]wb.SellerHit{7: {SupplierID: 7, Query: "куртка"}}}

	path := filepath.Join(dir, "sellers.csv")
	n, err := WriteCSVFile(path, res, cache)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("временный файл должен быть переименован")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(b, []byte("\xEF\xBB\xBF")))).ReadAll()
	if err != nil {
		t.Fatalf("CSV не читается обратно: %v", err)
	}
	if len(rows) != 2 || rows[1][1] != "ООО \"Кавычки\", и запятая" || rows[1][12] != "куртка" {
		t.Fatalf("rows = %q", rows)
	}

	// Ошибка создания файла (каталога нет) — ошибка, без мусора рядом.
	if _, err := WriteCSVFile(filepath.Join(dir, "nope", "x.csv"), res, cache); err == nil {
		t.Fatal("ожидалась ошибка")
	}
}
