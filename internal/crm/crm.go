// Пакет crm заливает продавцов в Twenty CRM по REST: компания, контакт и
// сделка с источником WB и заметкой-разбором. Дедуп по ИНН через amoId сделки
// `wb:<инн>`, поэтому повторный прогон и другие лидгены (SEO с `seo-outreach:`)
// не дублируют сделку. Браузера здесь нет: на вход идут уже собранные продавцы.
package crm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

// actor помечает всё, что создаёт этот коннектор, чтобы отличать от ручного и
// от SEO-рассылки в ленте CRM.
var actor = map[string]string{"source": "API", "name": "WB-лидген"}

// stage — стартовая стадия: «разобрать». Менеджер разбирает из неё.
const stage = "NERAZOBRANNOE"

// Lead — один продавец на вход коннектора, собранный из NDJSON go-wb.
type Lead struct {
	INN       string
	Name      string // сокращённое наименование / имя на витрине
	Director  string // руководитель или ИП, если известен
	Phones    []string
	Emails    []string
	Website   string
	Trademark string
	Query     string // поисковый запрос, по которому нашли
	Address   string
	Okved     string
	Status    string // «Действует» и т.п.
}

// Client — минимальный REST-клиент Twenty с ограничителем скорости: ключ делит
// лимит (~100 запросов в минуту) с другими писателями, поэтому запросы разводятся.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
	limiter *rate.Limiter
}

// New строит клиент. rps<=0 ставит безопасные 2 запроса в секунду.
func New(baseURL, apiKey string, rps float64) *Client {
	if rps <= 0 {
		rps = 2
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		limiter: rate.NewLimiter(rate.Limit(rps), 1),
	}
}

// amoID — метка сделки для дедупа: тот же приём, что у SEO (`seo-outreach:`).
func amoID(inn string) string { return "wb:" + inn }

// Result — что стало с одним лидом.
type Result struct {
	Created       bool
	OpportunityID string
}

// Import заводит лид в CRM, если сделки с таким ИНН ещё нет. Идемпотентно по
// amoId: повторный прогон вернёт Created=false и уже существующую сделку.
func (c *Client) Import(ctx context.Context, lead Lead) (Result, error) {
	inn := strings.TrimSpace(lead.INN)
	if inn == "" {
		return Result{}, fmt.Errorf("пустой ИНН: нечем дедупить")
	}

	if id, err := c.findOpportunity(ctx, amoID(inn)); err != nil {
		return Result{}, err
	} else if id != "" {
		return Result{OpportunityID: id}, nil
	}

	companyID, err := c.ensureCompany(ctx, lead)
	if err != nil {
		return Result{}, err
	}
	personID, err := c.ensurePerson(ctx, lead, companyID)
	if err != nil {
		return Result{}, err
	}

	opp := map[string]any{
		"name":      trunc("WB: "+firstNonEmpty(lead.Trademark, lead.Name, inn), 250),
		"stage":     stage,
		"amoId":     amoID(inn),
		"companyId": companyID,
		"createdBy": actor,
	}
	if personID != "" {
		opp["pointOfContactId"] = personID
	}
	var created struct{ ID string }
	if err := c.post(ctx, "/rest/opportunities", opp, &created); err != nil {
		return Result{}, err
	}

	if err := c.attachNote(ctx, created.ID, lead); err != nil {
		return Result{}, err
	}
	return Result{Created: true, OpportunityID: created.ID}, nil
}

func (c *Client) ensureCompany(ctx context.Context, lead Lead) (string, error) {
	domain := domainOf(lead.Website)
	if domain != "" {
		if id, err := c.findCompany(ctx, domain); err != nil {
			return "", err
		} else if id != "" {
			return id, nil
		}
	}
	body := map[string]any{
		"name":      trunc(firstNonEmpty(lead.Name, lead.Trademark, lead.INN), 200),
		"createdBy": actor,
	}
	if domain != "" {
		body["domainName"] = map[string]string{"primaryLinkUrl": "https://" + domain, "primaryLinkLabel": ""}
	}
	var created struct{ ID string }
	if err := c.post(ctx, "/rest/companies", body, &created); err != nil {
		return "", err
	}
	return created.ID, nil
}

func (c *Client) ensurePerson(ctx context.Context, lead Lead, companyID string) (string, error) {
	email := firstNonEmpty(lead.Emails...)
	if email != "" {
		if id, err := c.findPerson(ctx, email); err != nil {
			return "", err
		} else if id != "" {
			return id, nil
		}
	}
	// Контакт нужен, чтобы нести e-mail для КП и телефон для звонка. Имя
	// неизвестно — ставим директора или название как подпись.
	first, last := splitName(firstNonEmpty(lead.Director, lead.Name, lead.Trademark))
	body := map[string]any{
		"name":      map[string]string{"firstName": trunc(first, 100), "lastName": trunc(last, 100)},
		"companyId": companyID,
		"createdBy": actor,
	}
	if email != "" {
		body["emails"] = map[string]string{"primaryEmail": email}
	}
	if phone := firstNonEmpty(lead.Phones...); phone != "" {
		body["phones"] = map[string]string{"primaryPhoneNumber": phone}
	}
	// Без e-mail и телефона контакт бесполезен: пусть сделка висит без него.
	if email == "" && firstNonEmpty(lead.Phones...) == "" {
		return "", nil
	}
	var created struct{ ID string }
	if err := c.post(ctx, "/rest/people", body, &created); err != nil {
		return "", err
	}
	return created.ID, nil
}

func (c *Client) attachNote(ctx context.Context, opportunityID string, lead Lead) error {
	var note struct{ ID string }
	if err := c.post(ctx, "/rest/notes", map[string]any{
		"title":     trunc("WB-разбор: "+firstNonEmpty(lead.Trademark, lead.Name, lead.INN), 200),
		"bodyV2":    map[string]string{"markdown": noteMarkdown(lead)},
		"createdBy": actor,
	}, &note); err != nil {
		return err
	}
	return c.post(ctx, "/rest/noteTargets", map[string]any{
		"noteId":              note.ID,
		"targetOpportunityId": opportunityID,
	}, nil)
}

// noteMarkdown — разбор продавца; наш текст, обычный markdown, без длинного тире.
func noteMarkdown(lead Lead) string {
	lines := []string{"**WB-продавец**"}
	add := func(label, value string) {
		if strings.TrimSpace(value) != "" {
			lines = append(lines, "- "+label+": "+value)
		}
	}
	add("Бренд", lead.Trademark)
	add("Запрос", lead.Query)
	add("ИНН", lead.INN)
	add("Директор", lead.Director)
	add("Статус", lead.Status)
	add("ОКВЭД", lead.Okved)
	add("Адрес", lead.Address)
	add("Сайт", lead.Website)
	return strings.Join(lines, "\n")
}

// ----------------------------------------------------------- transport ----

func (c *Client) findOpportunity(ctx context.Context, amo string) (string, error) {
	return c.findFirst(ctx, "opportunities", fmt.Sprintf("amoId[eq]:%q", amo))
}

func (c *Client) findCompany(ctx context.Context, domain string) (string, error) {
	// Ссылки хранятся по-разному; совпадение нестрогое, этого для дедупа хватает.
	return c.findFirst(ctx, "companies", fmt.Sprintf("domainName.primaryLinkUrl[like]:%q", "%"+domain+"%"))
}

func (c *Client) findPerson(ctx context.Context, email string) (string, error) {
	return c.findFirst(ctx, "people", fmt.Sprintf("emails.primaryEmail[eq]:%q", email))
}

// findFirst возвращает id первой записи объекта под фильтром или "" если нет.
func (c *Client) findFirst(ctx context.Context, object, filter string) (string, error) {
	path := fmt.Sprintf("/rest/%s?filter=%s&limit=1&depth=0", object, url.QueryEscape(filter))
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return "", err
	}
	rows := firstList(raw)
	if len(rows) == 0 {
		return "", nil
	}
	var row struct{ ID string }
	if err := json.Unmarshal(rows[0], &row); err != nil {
		return "", err
	}
	return row.ID, nil
}

func (c *Client) post(ctx context.Context, path string, body, out any) error {
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodPost, path, body, &raw); err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if first := firstValue(raw); first != nil {
		return json.Unmarshal(first, out)
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	if err := c.limiter.Wait(ctx); err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("CRM %s %s: HTTP %d %s", method, strings.Split(path, "?")[0], resp.StatusCode, trunc(string(payload), 300))
	}
	if out != nil {
		var envelope struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(payload, &envelope); err != nil {
			return err
		}
		if raw, ok := out.(*json.RawMessage); ok {
			*raw = envelope.Data
		}
	}
	return nil
}

// firstValue берёт первое значение из {data:{createX:{...}}}.
func firstValue(data json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	for _, v := range m {
		return v
	}
	return nil
}

// firstList берёт первый массив из {data:{opportunities:[...]}}.
func firstList(data json.RawMessage) []json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	for _, v := range m {
		var rows []json.RawMessage
		if json.Unmarshal(v, &rows) == nil {
			return rows
		}
	}
	return nil
}

// ---------------------------------------------------------------- utils ----

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func trunc(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// splitName делит ФИО на имя и фамилию грубо: первое слово — фамилия, остальное
// — имя. Twenty всё равно показывает оба поля рядом.
func splitName(full string) (first, last string) {
	parts := strings.Fields(full)
	switch len(parts) {
	case 0:
		return "", ""
	case 1:
		return parts[0], ""
	default:
		return strings.Join(parts[1:], " "), parts[0]
	}
}

// domainOf вытаскивает голый домен из URL или пустую строку.
func domainOf(website string) string {
	s := strings.TrimSpace(strings.ToLower(website))
	if s == "" {
		return ""
	}
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "www.")
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return s
}
