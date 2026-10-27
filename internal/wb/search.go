package wb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/megamen32/site-mimic/mimic"
)

// ErrChallenge означает, что WB ответил страницей JS-антибота (498).
// Мы здесь намеренно останавливаемся и не пытаемся её обойти.
var ErrChallenge = errors.New("search.wb.ru вернул 498 (JS-проверка), остановка")

// defaultSearchBaseURL — поисковый API WB; в тестах подменяется на httptest-сервер.
const defaultSearchBaseURL = "https://search.wb.ru"

// maxSearchBody — предел размера страницы выдачи. Реальные ответы — сотни килобайт.
const maxSearchBody = 16 << 20

// statusChallenge — нестандартный код WB для JS-проверки антибота.
const statusChallenge = 498

// Шарды возвращают одинаковые результаты; их ротация помогает при 429.
var shards = []string{"common", "male", "female", "sport", "kids", "beauty", "electronic", "household"}

type product struct {
	ID             int     `json:"id"`
	Supplier       string  `json:"supplier"`
	SupplierID     int     `json:"supplierId"`
	SupplierRating float64 `json:"supplierRating"`
}

// Старые версии API заворачивают товары в "data"; обрабатываем обе формы.
type searchResp struct {
	Products []product `json:"products"`
	Data     struct {
		Products []product `json:"products"`
	} `json:"data"`
}

func (r searchResp) items() []product {
	if len(r.Products) > 0 {
		return r.Products
	}
	return r.Data.Products
}

// Searcher обращается к поисковому API WB через клиент site-mimic.
// Небезопасен для конкурентного доступа: shardIdx и streak429 — обычные поля.
type Searcher struct {
	client    *http.Client
	profile   mimic.Profile
	baseURL   string
	dest      string
	version   string
	minDelay  time.Duration // пауза между страницами, от
	maxDelay  time.Duration // и до (случайно в этом диапазоне)
	retryWait time.Duration // пауза после 429 или сетевой ошибки
	longWait  time.Duration // пауза после серии из 429

	// DumpPath, если задан, получает первый сырой ответ 200 и затем очищается.
	DumpPath string

	shardIdx  int
	streak429 int
}

func NewSearcher(profilePath, proxy, dest, version string) (*Searcher, error) {
	p, err := mimic.LoadProfile(profilePath)
	if err != nil {
		return nil, err
	}
	var opts []mimic.Option
	if proxy != "" {
		opts = append(opts, mimic.WithProxy(proxy))
	}
	c, err := mimic.New(p, opts...)
	if err != nil {
		return nil, err
	}
	c.Timeout = 30 * time.Second
	return &Searcher{
		client: c, profile: p, baseURL: defaultSearchBaseURL, dest: dest, version: version,
		minDelay: 1 * time.Second, maxDelay: 2 * time.Second,
		retryWait: 2 * time.Second, longWait: 30 * time.Second,
	}, nil
}

func (s *Searcher) pause(ctx context.Context) error {
	return Sleep(ctx, s.minDelay+rand.N(s.maxDelay-s.minDelay+1))
}

func (s *Searcher) url(shard, query string, page int) string {
	q := url.Values{}
	q.Set("ab_testing", "false")
	q.Set("appType", "1")
	q.Set("curr", "rub")
	q.Set("dest", s.dest)
	q.Set("lang", "ru")
	q.Set("page", strconv.Itoa(page))
	q.Set("query", query)
	q.Set("resultset", "catalog")
	q.Set("sort", "popular")
	q.Set("spp", "30")
	return fmt.Sprintf("%s/exactmatch/ru/%s/%s/search?%s", s.baseURL, shard, s.version, q.Encode())
}

// fetchPage возвращает товары одной страницы. Обрабатывает 429 ротацией шардов.
func (s *Searcher) fetchPage(ctx context.Context, query string, page int) ([]product, error) {
	const maxRetries = 2
	for attempt := 0; ; attempt++ {
		shard := shards[s.shardIdx%len(shards)]
		req, err := s.profile.Request(http.MethodGet, s.url(shard, query, page), nil)
		if err != nil {
			return nil, err
		}
		resp, err := s.client.Do(req.WithContext(ctx))
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if attempt < maxRetries {
				slog.Warn("сеть, повтор", "err", err)
				if err := Sleep(ctx, s.retryWait); err != nil {
					return nil, err
				}
				continue
			}
			return nil, err
		}
		body, err := readBody(resp, maxSearchBody)
		_ = resp.Body.Close() // ошибка закрытия тела ответа ничего не добавляет к ошибке чтения
		if err != nil {
			return nil, fmt.Errorf("чтение ответа: %w", err)
		}

		switch resp.StatusCode {
		case http.StatusOK:
			s.streak429 = 0
			if s.DumpPath != "" {
				if err := os.WriteFile(s.DumpPath, body, 0o644); err != nil {
					slog.Warn("не удалось сохранить сырой ответ", "path", s.DumpPath, "err", err)
				}
				s.DumpPath = ""
			}
			var r searchResp
			if err := json.Unmarshal(body, &r); err != nil {
				return nil, fmt.Errorf("разбор JSON: %w (начало: %.200s)", err, body)
			}
			return r.items(), nil

		case statusChallenge:
			return nil, ErrChallenge

		case http.StatusTooManyRequests:
			s.streak429++
			s.shardIdx++
			if attempt >= maxRetries {
				return nil, fmt.Errorf("429 после %d повторов", maxRetries)
			}
			wait := s.retryWait
			if s.streak429 >= 5 {
				slog.Warn("серия 429, длинная пауза", "streak", s.streak429)
				wait = s.longWait
				s.streak429 = 0
			}
			if err := Sleep(ctx, wait); err != nil {
				return nil, err
			}

		default:
			return nil, fmt.Errorf("статус %d (начало: %.200s)", resp.StatusCode, body)
		}
	}
}

// Collect идёт по страницам одного запроса, пока не кончатся товары или не достигнет maxPages.
func (s *Searcher) Collect(ctx context.Context, query string, maxPages int, emit func(SellerHit)) error {
	for page := 1; page <= maxPages; page++ {
		items, err := s.fetchPage(ctx, query, page)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			break
		}
		for _, p := range items {
			if p.SupplierID == 0 {
				continue
			}
			emit(SellerHit{SupplierID: p.SupplierID, Supplier: p.Supplier, SupplierRating: p.SupplierRating, Query: query})
		}
		slog.Info("страница", "query", query, "page", page, "items", len(items))
		if err := s.pause(ctx); err != nil {
			return err
		}
	}
	return nil
}
