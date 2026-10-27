package collect

import (
	"encoding/json"
	"io"
	"sync"

	"github.com/OkaneKatsuro/go-wb/internal/wb"
)

// ndjsonRecord — плоская запись одного продавца для NDJSON: юрданные WB плюс
// обогащение. По объекту на строку, удобно читать потоково из другого процесса.
type ndjsonRecord struct {
	SupplierID   int            `json:"supplierId"`
	Name         string         `json:"supplierName,omitempty"`
	FullName     string         `json:"supplierFullName,omitempty"`
	INN          string         `json:"inn,omitempty"`
	OGRN         string         `json:"ogrn,omitempty"`
	KPP          string         `json:"kpp,omitempty"`
	LegalAddress string         `json:"legalAddress,omitempty"`
	Trademark    string         `json:"trademark,omitempty"`
	Rating       float64        `json:"supplierRating,omitempty"`
	Deactivated  bool           `json:"deactivated"`
	NotFound     bool           `json:"notFound,omitempty"`
	Query        string         `json:"sourceQuery,omitempty"`
	Enrich       *wb.Enrichment `json:"enrich,omitempty"`
}

func flatten(rec Record) ndjsonRecord {
	s := rec.Supplier
	if s == nil {
		s = &wb.Supplier{ID: rec.Hit.SupplierID}
	}
	name := s.Name
	if name == "" {
		name = rec.Hit.Supplier
	}
	return ndjsonRecord{
		SupplierID: s.ID, Name: name, FullName: s.FullName, INN: s.INN,
		OGRN: s.OGRN, KPP: s.KPP, LegalAddress: s.LegalAddress, Trademark: s.Trademark,
		Rating: rec.Hit.SupplierRating, Deactivated: s.Deactivated, NotFound: s.NotFound,
		Query: rec.Hit.Query, Enrich: s.Enrich,
	}
}

// NDJSONWriter пишет записи в формате NDJSON (один JSON-объект на строку).
// Потокобезопасен: воркеры зовут Write параллельно по мере готовности продавцов.
// Пиши прямо в os.Stdout (без буфера), чтобы другой процесс получал строки сразу.
type NDJSONWriter struct {
	mu  sync.Mutex
	enc *json.Encoder
	n   int
}

func NewNDJSONWriter(w io.Writer) *NDJSONWriter {
	return &NDJSONWriter{enc: json.NewEncoder(w)}
}

// Write кодирует одну запись и завершает её переводом строки (это делает Encode).
func (nw *NDJSONWriter) Write(rec Record) {
	nw.mu.Lock()
	defer nw.mu.Unlock()
	if err := nw.enc.Encode(flatten(rec)); err == nil {
		nw.n++
	}
}

// Count — сколько строк записано.
func (nw *NDJSONWriter) Count() int {
	nw.mu.Lock()
	defer nw.mu.Unlock()
	return nw.n
}
