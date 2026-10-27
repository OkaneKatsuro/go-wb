package wb

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// maxLineBytes — предел длины одной строки ввода. Строка длиннее — не id и не
// запрос, а мусор; ParseLines вернёт bufio.ErrTooLong вместо тихого обрыва.
const maxLineBytes = 1 << 20

// ReadLines возвращает значимые строки текстового файла: обрезанные по краям,
// непустые, без строк, начинающихся с #.
func ReadLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	lines, err := ParseLines(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return lines, err
}

// ParseLines — это ReadLines поверх любого reader (файл, поле HTTP-формы, строка).
func ParseLines(r io.Reader) ([]string, error) {
	var res []string
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, maxLineBytes)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l != "" && !strings.HasPrefix(l, "#") {
			res = append(res, l)
		}
	}
	return res, sc.Err()
}

// ParseIDs превращает строки в supplierId. Строка может быть голым числом или
// ссылкой на продавца вида https://www.wildberries.ru/seller/12345?sort=popular.
// Строки, которые не то и не другое, пропускаются.
func ParseIDs(lines []string) []int {
	ids := make([]int, 0, len(lines))
	for _, l := range lines {
		if id := parseID(l); id > 0 {
			ids = append(ids, id)
		}
	}
	return ids
}

func parseID(s string) int {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimRight(s, "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	id, err := strconv.Atoi(s)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// Имена колонок CSV, которые понимает ExtractHits (без учёта регистра):
// supplier_id обязательна, остальные подхватываются, если есть.
var (
	idColumns    = map[string]bool{"supplier_id": true, "supplierid": true}
	nameColumns  = map[string]bool{"supplier": true, "suppliername": true, "supplier_name": true}
	queryColumns = map[string]bool{"query": true, "source_query": true, "sourcequery": true}
)

// HitList — это Source поверх готового списка найденных продавцов,
// например из CSV cmd/wb-search: в отличие от IDList несёт имя и запрос.
type HitList []SellerHit

func (l HitList) Collect(ctx context.Context, emit func(SellerHit)) error {
	for _, h := range l {
		if err := ctx.Err(); err != nil {
			return err
		}
		if h.SupplierID > 0 {
			emit(h)
		}
	}
	return nil
}

// ExtractHits читает продавцов из текста любого из двух видов: по одному id или
// ссылке на строку (см. ParseIDs) либо CSV с колонкой supplier_id — так выглядит
// вывод cmd/wb-search, и его можно подать как есть; из него же берутся имя
// продавца и запрос, если такие колонки есть. Дубли не убираются, это делает Runner.
func ExtractHits(r io.Reader) ([]SellerHit, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	b = bytes.TrimPrefix(b, []byte("\xEF\xBB\xBF")) // BOM из Excel или нашего же CSV
	if cols, ok := csvColumns(b); ok {
		return parseCSVHits(b, cols)
	}
	lines, err := ParseLines(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	ids := ParseIDs(lines)
	hits := make([]SellerHit, len(ids))
	for i, id := range ids {
		hits[i] = SellerHit{SupplierID: id}
	}
	return hits, nil
}

// ReadHits — это ExtractHits поверх файла.
func ReadHits(path string) ([]SellerHit, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	hits, err := ExtractHits(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return hits, err
}

// csvCols — номера нужных колонок в CSV; -1, если колонки нет.
type csvCols struct{ id, name, query int }

// csvColumns распознаёт заголовок CSV с колонкой supplier_id в первой строке.
func csvColumns(b []byte) (csvCols, bool) {
	cols := csvCols{id: -1, name: -1, query: -1}
	first, _, _ := bytes.Cut(b, []byte("\n"))
	if !bytes.Contains(first, []byte(",")) {
		return cols, false
	}
	header, err := csv.NewReader(bytes.NewReader(first)).Read()
	if err != nil {
		return cols, false
	}
	for i, h := range header {
		h = strings.ToLower(strings.TrimSpace(h))
		switch {
		case idColumns[h] && cols.id < 0:
			cols.id = i
		case nameColumns[h] && cols.name < 0:
			cols.name = i
		case queryColumns[h] && cols.query < 0:
			cols.query = i
		}
	}
	return cols, cols.id >= 0
}

func parseCSVHits(b []byte, cols csvCols) ([]SellerHit, error) {
	cr := csv.NewReader(bytes.NewReader(b))
	cr.FieldsPerRecord = -1 // строки с разным числом полей не считаем ошибкой
	cr.LazyQuotes = true
	records, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("разбор CSV: %w", err)
	}
	field := func(rec []string, i int) string {
		if i >= 0 && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	hits := make([]SellerHit, 0, len(records))
	for _, rec := range records[1:] { // [0] — заголовок
		id := parseID(field(rec, cols.id))
		if id <= 0 {
			continue
		}
		hits = append(hits, SellerHit{SupplierID: id, Supplier: field(rec, cols.name), Query: field(rec, cols.query)})
	}
	return hits, nil
}
