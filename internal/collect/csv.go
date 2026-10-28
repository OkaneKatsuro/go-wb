package collect

import (
	"encoding/csv"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/OkaneKatsuro/go-wb/internal/store"
	"github.com/OkaneKatsuro/go-wb/internal/wb"
)

var (
	baseHeader = []string{
		"supplierId", "supplierName", "supplierFullName", "inn", "ogrn", "kpp",
		"legalAddress", "trademark", "taxpayerCode", "supplierRating", "deactivated", "notFound", "source_query",
		"sellerType",
	}
	// колонки обогащения (OFData), пустые без -enrich; ширина строки от них не зависит
	enrichHeader = []string{
		"of_kind", "of_shortName", "of_fullName", "of_ogrn", "of_status", "of_regDate",
		"of_address", "of_okvedCode", "of_okvedName", "of_director",
		"of_phones", "of_emails", "of_website", // несколько значений через «; »
	}
	csvHeader = append(append([]string{}, baseHeader...), enrichHeader...)
)

// CSVSafe защищает от CSV-инъекции: имя продавца приходит от WB, и если ячейка
// начинается с =, +, -, @, таба или возврата каретки, Excel может выполнить её
// как формулу. Префикс ' обезвреживает такую ячейку. Отрицательные числа
// («-20») формулой не являются и остаются как есть.
func CSVSafe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '-':
		if isDecimal(s[1:]) {
			return s
		}
		return "'" + s
	case '=', '+', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

// isDecimal — обычное десятичное число: цифры и не больше одной точки.
// strconv.ParseFloat здесь не годится: он принимает «-inf», «-NaN» и
// шестнадцатеричные «-0x1p-2», которые Excel посчитал бы формулой.
func isDecimal(s string) bool {
	digits, dots := 0, 0
	for i := range len(s) {
		switch {
		case s[i] >= '0' && s[i] <= '9':
			digits++
		case s[i] == '.':
			dots++
		default:
			return false
		}
	}
	return digits > 0 && dots <= 1
}

// ExcelWriter пишет CSV, который Excel откроет правильно: BOM для кириллицы
// и CSVSafe для каждой ячейки. Все CSV проекта должны идти через него, чтобы
// защита от инъекции не зависела от памяти автора очередного экспорта.
type ExcelWriter struct {
	cw *csv.Writer
}

// NewExcelWriter сразу пишет BOM в w.
func NewExcelWriter(w io.Writer) (*ExcelWriter, error) {
	if _, err := io.WriteString(w, "\xEF\xBB\xBF"); err != nil {
		return nil, err
	}
	return &ExcelWriter{cw: csv.NewWriter(w)}, nil
}

// Write обезвреживает ячейки и пишет строку; row не изменяется.
func (e *ExcelWriter) Write(row []string) error {
	safe := make([]string, len(row))
	for i, c := range row {
		safe[i] = CSVSafe(c)
	}
	return e.cw.Write(safe)
}

// Flush сбрасывает буфер и возвращает первую ошибку записи.
func (e *ExcelWriter) Flush() error {
	e.cw.Flush()
	return e.cw.Error()
}

// WriteCSV пишет по одной строке на id в порядке res.IDs.
func WriteCSV(w io.Writer, res *Result, cache *store.Cache) (int, error) {
	ew, err := NewExcelWriter(w)
	if err != nil {
		return 0, err
	}
	if err := ew.Write(csvHeader); err != nil {
		return 0, err
	}
	n := 0
	for _, id := range res.IDs {
		if err := ew.Write(csvRow(id, res.Hits[id], cache)); err != nil {
			return n, err
		}
		n++
	}
	return n, ew.Flush()
}

func csvRow(id int, h wb.SellerHit, cache *store.Cache) []string {
	s, ok := cache.Get(id)
	if !ok {
		s = &wb.Supplier{ID: id}
	}
	name := s.Name
	if name == "" {
		name = h.Supplier
	}
	rating := ""
	if h.SupplierRating != 0 {
		rating = strconv.FormatFloat(h.SupplierRating, 'f', -1, 64)
	}
	row := make([]string, 0, len(csvHeader))
	row = append(row,
		strconv.Itoa(id), name, s.FullName, s.INN, s.OGRN, s.KPP,
		s.LegalAddress, s.Trademark, s.TaxpayerCode, rating,
		strconv.FormatBool(s.Deactivated), strconv.FormatBool(s.NotFound), h.Query, s.SellerType,
	)
	return append(row, enrichColumns(s.Enrich)...)
}

// enrichColumns отдаёт значения колонок обогащения; без обогащения или при
// «не найдено» — пустые строки, чтобы ширина строки CSV была постоянной.
func enrichColumns(e *wb.Enrichment) []string {
	if e == nil || e.NotFound {
		return make([]string, len(enrichHeader))
	}
	return []string{
		e.Kind, e.ShortName, e.FullName, e.OGRN, e.Status, e.RegDate,
		e.Address, e.OkvedCode, e.OkvedName, e.Director,
		strings.Join(e.Phones, "; "), strings.Join(e.Emails, "; "), e.Website,
	}
}

// WriteCSVFile — это WriteCSV в файл. Пишет во временный файл рядом и
// переименовывает, поэтому по пути path никогда не лежит недописанный CSV.
func WriteCSVFile(path string, res *Result, cache *store.Cache) (int, error) {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	n, err := WriteCSV(f, res, cache)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	return n, nil
}

// WriteIDs пишет отсортированные id, по одному на строку.
func WriteIDs(path string, res *Result) error {
	var b strings.Builder
	for _, id := range res.IDs {
		b.WriteString(strconv.Itoa(id))
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
