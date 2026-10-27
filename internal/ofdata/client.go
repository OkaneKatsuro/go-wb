// Пакет ofdata обогащает продавца данными из реестров ЕГРЮЛ/ЕГРИП
// через API OFData (https://api.ofdata.ru). Ключ передаётся снаружи,
// в коде его нет.
package ofdata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/OkaneKatsuro/go-wb/internal/wb"
)

const defaultBaseURL = "https://api.ofdata.ru/v2"

// Client обращается к OFData с общим ограничением скорости. Безопасен для горутин.
type Client struct {
	key     string
	BaseURL string
	Backoff time.Duration

	http *http.Client
	tick *time.Ticker
}

// New создаёт клиент. key обязателен (OFDATA_KEY). rps — запросов в секунду.
func New(key string, rps int) (*Client, error) {
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("нужен ключ OFData (переменная OFDATA_KEY или флаг -ofdata-key)")
	}
	if rps <= 0 {
		rps = 3
	}
	return &Client{
		key:     key,
		BaseURL: defaultBaseURL,
		Backoff: 2 * time.Second,
		http:    &http.Client{Timeout: 25 * time.Second},
		tick:    time.NewTicker(time.Second / time.Duration(rps)),
	}, nil
}

// apiResp повторяет форму ответа OFData: блоки data и meta.
// Разобраны только нужные поля; остальное игнорируется.
type apiResp struct {
	Data struct {
		ОГРН     string    `json:"ОГРН"`
		ОГРНИП   string    `json:"ОГРНИП"`
		НаимПолн string    `json:"НаимПолн"`
		НаимСокр string    `json:"НаимСокр"`
		ФИО      string    `json:"ФИО"`
		ТипСокр  string    `json:"ТипСокр"`
		ДатаРег  string    `json:"ДатаРег"`
		Статус   kodNaim   `json:"Статус"`
		ЮрАдрес  *адрес    `json:"ЮрАдрес"`
		Регион   kodNaim   `json:"Регион"`
		ОКВЭД    kodNaim   `json:"ОКВЭД"`
		ОКОПФ    kodNaim   `json:"ОКОПФ"`
		Руковод  []руковод `json:"Руковод"`
	} `json:"data"`
	Meta struct {
		Status  string  `json:"status"`
		Balance float64 `json:"balance"`
	} `json:"meta"`
}

type kodNaim struct {
	Код  string `json:"Код"`
	Наим string `json:"Наим"`
}
type адрес struct {
	АдресРФ string `json:"АдресРФ"`
}
type руковод struct {
	ФИО string `json:"ФИО"`
}

// Enrich возвращает обогащение по ИНН или (nil, nil), если запись не найдена.
// Эндпоинт выбирается по длине ИНН: 12 цифр — ИП, иначе юрлицо.
func (c *Client) Enrich(ctx context.Context, inn string) (*wb.Enrichment, error) {
	inn = strings.TrimSpace(inn)
	if inn == "" {
		return nil, nil
	}
	method := "company"
	if wb.IsIP(inn) {
		method = "entrepreneur"
	}

	const maxRetries = 3
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.tick.C:
		}
		e, retry, err := c.fetch(ctx, method, inn)
		if err == nil {
			return e, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		lastErr = err
		if !retry {
			break
		}
		t := time.NewTimer(time.Duration(attempt+1) * c.Backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
	return nil, fmt.Errorf("ofdata %s инн=%s: %w", method, inn, lastErr)
}

func (c *Client) fetch(ctx context.Context, method, inn string) (*wb.Enrichment, bool, error) {
	q := url.Values{}
	q.Set("key", c.key)
	q.Set("inn", inn)
	u := c.BaseURL + "/" + method + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, false, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, true, redactURLError(err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		var r apiResp
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			return nil, false, fmt.Errorf("разбор JSON: %w", err)
		}
		if r.Meta.Status != "ok" || (r.Data.ОГРН == "" && r.Data.ОГРНИП == "") {
			return nil, false, nil // запись не найдена — не ошибка
		}
		return mapEnrichment(method, &r), false, nil
	case resp.StatusCode == http.StatusNotFound:
		return nil, false, nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return nil, true, fmt.Errorf("статус %d", resp.StatusCode)
	default:
		return nil, false, fmt.Errorf("статус %d", resp.StatusCode)
	}
}

// redactURLError убирает URL из ошибки http.Client.Do. В URL лежит key
// в query-строке, а *url.Error печатает весь URL, поэтому в логи он попасть не должен.
// Оставляем только операцию и внутреннюю причину (dial, timeout и т.п.).
func redactURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}

func mapEnrichment(method string, r *apiResp) *wb.Enrichment {
	d := r.Data
	e := &wb.Enrichment{
		Okopf:      d.ОКОПФ.Наим,
		Status:     d.Статус.Наим,
		StatusCode: d.Статус.Код,
		RegDate:    d.ДатаРег,
		Region:     d.Регион.Наим,
		OkvedCode:  d.ОКВЭД.Код,
		OkvedName:  d.ОКВЭД.Наим,
		Source:     "ofdata",
	}
	if method == "entrepreneur" {
		e.Kind = "ИП"
		e.OGRN = d.ОГРНИП
		e.FullName = d.ФИО
		e.ShortName = strings.TrimSpace(d.ТипСокр + " " + d.ФИО)
		e.Director = d.ФИО
		e.Address = d.Регион.Наим // у ИП адрес не публикуется, берём регион
	} else {
		e.Kind = "ЮЛ"
		e.OGRN = d.ОГРН
		e.FullName = d.НаимПолн
		e.ShortName = d.НаимСокр
		if d.ЮрАдрес != nil {
			e.Address = d.ЮрАдрес.АдресРФ
		}
		if len(d.Руковод) > 0 {
			e.Director = d.Руковод[0].ФИО
		}
	}
	return e
}
