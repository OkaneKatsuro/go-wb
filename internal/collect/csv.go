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

var csvHeader = []string{
	"supplierId", "supplierName", "supplierFullName", "inn", "ogrn", "kpp",
	"legalAddress", "trademark", "taxpayerCode", "supplierRating", "deactivated", "notFound", "source_query",
}

// csvSafe защищает от CSV-инъекции: имя продавца приходит от WB, и если ячейка
// начинается с =, +, -, @, таба или возврата каретки, Excel может выполнить её
// как формулу. Префикс ' обезвреживает такую ячейку.
func csvSafe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

// WriteCSV пишет по одной строке на id. Начинает с BOM, чтобы Excel правильно открыл кириллицу.
func WriteCSV(w io.Writer, res *Result, cache *store.Cache) (int, error) {
	if _, err := io.WriteString(w, "\xEF\xBB\xBF"); err != nil {
		return 0, err
	}
	cw := csv.NewWriter(w)
	if err := cw.Write(csvHeader); err != nil {
		return 0, err
	}
	n := 0
	for _, id := range res.IDs {
		h := res.Hits[id]
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
		row := []string{
			strconv.Itoa(id), name, s.FullName, s.INN, s.OGRN, s.KPP,
			s.LegalAddress, s.Trademark, s.TaxpayerCode, rating,
			strconv.FormatBool(s.Deactivated), strconv.FormatBool(s.NotFound), h.Query,
		}
		for i := range row {
			row[i] = csvSafe(row[i])
		}
		if err := cw.Write(row); err != nil {
			return n, err
		}
		n++
	}
	cw.Flush()
	return n, cw.Error()
}

// WriteCSVFile — это WriteCSV в заново созданный файл.
func WriteCSVFile(path string, res *Result, cache *store.Cache) (int, error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	n, err := WriteCSV(f, res, cache)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return n, err
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
