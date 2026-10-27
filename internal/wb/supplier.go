package wb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// DefaultBaseURL — публичный CDN WB с юридическими данными продавцов.
const DefaultBaseURL = "https://static-basket-01.wbbasket.ru"

// Supplier — юридические данные одного продавца WB в том виде, в каком их отдаёт CDN:
// {BaseURL}/vol{id/1000}/data/supplier-by-id/{id}.json
type Supplier struct {
	ID           int    `json:"supplierId"`
	Name         string `json:"supplierName"`
	FullName     string `json:"supplierFullName"`
	INN          string `json:"inn"`
	OGRN         string `json:"ogrn"`
	OGRNIP       string `json:"ogrnip,omitempty"`
	KPP          string `json:"kpp"`
	LegalAddress string `json:"legalAddress"`
	Trademark    string `json:"trademark"`
	TaxpayerCode string `json:"taxpayerCode"`
	UNP          string `json:"unp,omitempty"` // Беларусь
	BIN          string `json:"bin,omitempty"` // Казахстан
	UNN          string `json:"unn,omitempty"`
	Deactivated  bool   `json:"deactivated"`
	// NotFound мы ставим сами, когда CDN отвечает 404, чтобы не запрашивать этот id снова.
	NotFound bool `json:"notFound,omitempty"`
	// Enrich — обогащение из внешнего реестра (OFData), nil пока не запрашивали.
	// Хранится прямо в Supplier, поэтому кэш сохраняет его между запусками.
	Enrich *Enrichment `json:"enrich,omitempty"`
}

// Enrichment — нормализованные данные из реестра (ЕГРЮЛ/ЕГРИП через OFData),
// одинаковые по форме для юрлица и ИП.
type Enrichment struct {
	Kind       string `json:"kind"`                 // "ЮЛ" или "ИП"
	Okopf      string `json:"okopf,omitempty"`      // организационно-правовая форма
	FullName   string `json:"fullName,omitempty"`   // полное наименование / ФИО
	ShortName  string `json:"shortName,omitempty"`  // сокращённое наименование
	OGRN       string `json:"ogrn,omitempty"`       // ОГРН или ОГРНИП
	Status     string `json:"status,omitempty"`     // «Действует», «Ликвидировано» и т.п.
	StatusCode string `json:"statusCode,omitempty"` // код статуса
	RegDate    string `json:"regDate,omitempty"`    // дата регистрации
	Address    string `json:"address,omitempty"`    // юр. адрес (ЮЛ) или регион (ИП)
	Region     string `json:"region,omitempty"`     // регион
	OkvedCode  string `json:"okvedCode,omitempty"`  // основной ОКВЭД, код
	OkvedName  string `json:"okvedName,omitempty"`  // основной ОКВЭД, название
	Director   string `json:"director,omitempty"`   // руководитель (ЮЛ) или сам ИП
	Source     string `json:"source"`               // источник, напр. "ofdata"
}

// IsIP сообщает, что ИНН принадлежит индивидуальному предпринимателю.
// У ИП ИНН из 12 цифр, у юрлица — из 10.
func IsIP(inn string) bool { return len(inn) == 12 }

// SupplierClient получает записи Supplier с общим ограничением скорости.
// Безопасен для конкурентного доступа.
type SupplierClient struct {
	// BaseURL — корень CDN. В тестах указывает на httptest-сервер.
	BaseURL string
	// Backoff — пауза перед первым повтором; дальше растёт линейно.
	Backoff time.Duration

	http *http.Client
	tick *time.Ticker // один тик = право на один запрос, общий для всех воркеров
}

func NewSupplierClient(rps int) *SupplierClient {
	if rps <= 0 {
		rps = 1
	}
	return &SupplierClient{
		BaseURL: DefaultBaseURL,
		Backoff: 2 * time.Second,
		http:    &http.Client{Timeout: 20 * time.Second},
		tick:    time.NewTicker(time.Second / time.Duration(rps)),
	}
}

func (c *SupplierClient) url(id int) string {
	return fmt.Sprintf("%s/vol%d/data/supplier-by-id/%d.json", c.BaseURL, id/1000, id)
}

// Get возвращает продавца или заглушку NotFound при 404.
// Сетевые ошибки, 429 и 5xx повторяются с растущей паузой.
func (c *SupplierClient) Get(ctx context.Context, id int) (*Supplier, error) {
	const maxRetries = 3
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.tick.C:
		}
		s, retry, err := c.fetch(ctx, id)
		if err == nil {
			return s, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		lastErr = err
		if !retry {
			break
		}
		if err := sleep(ctx, time.Duration(attempt+1)*c.Backoff); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("supplier %d: %w", id, lastErr)
}

// fetch делает один HTTP-запрос. Булев результат говорит, стоит ли повторять при ошибке.
func (c *SupplierClient) fetch(ctx context.Context, id int) (*Supplier, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(id), nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "wb-sellers/1.0")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, true, err
	}
	body, err := readBody(resp)
	resp.Body.Close()
	if err != nil {
		return nil, true, fmt.Errorf("чтение ответа: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		s := &Supplier{}
		if err := json.Unmarshal(body, s); err != nil {
			return nil, false, fmt.Errorf("разбор JSON: %w (начало: %.200s)", err, body)
		}
		s.ID = id
		if s.OGRN == "" {
			s.OGRN = s.OGRNIP // у ИП номер лежит в отдельном поле
		}
		return s, false, nil
	case resp.StatusCode == http.StatusNotFound:
		return &Supplier{ID: id, NotFound: true}, false, nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return nil, true, fmt.Errorf("статус %d", resp.StatusCode)
	default:
		return nil, false, fmt.Errorf("статус %d (начало: %.200s)", resp.StatusCode, body)
	}
}
