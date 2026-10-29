// Пакет storefront ищет товары на витрине Wildberries через настоящий браузер:
// открывает страницу поиска как обычный пользователь, дожидается сессии (кука
// x_wbaas_token) и запрашивает страницы выдачи fetch-ом изнутри той же
// страницы, поэтому заголовки и cookie подставляет сам браузер. Поверх пакета
// работают команда wb-search (CSV) и сервис wb-searchd (JSON для pipeline).
package storefront

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/stealth"
)

const searchURL = "https://www.wildberries.ru/__internal/u-search/exactmatch/ru/common/v18/search"

// tokenTimeout — сколько ждать куку x_wbaas_token после открытия страницы поиска.
const tokenTimeout = 30 * time.Second

// Response — ответ поиска витрины, только нужные поля.
type Response struct {
	Error    string    `json:"error"`
	Code     int       `json:"code"`
	Products []Product `json:"products"`
	Data     *struct {
		Products []Product `json:"products"`
	} `json:"data"`
}

func (r Response) items() []Product {
	if len(r.Products) > 0 {
		return r.Products
	}
	if r.Data != nil {
		return r.Data.Products
	}
	return nil
}

// Product — товар в выдаче витрины, имена полей как у WB.
type Product struct {
	ID              int64   `json:"id"`
	Name            string  `json:"name"`
	Entity          string  `json:"entity"`
	Brand           string  `json:"brand"`
	BrandID         int64   `json:"brandId"`
	Supplier        string  `json:"supplier"`
	SupplierID      int64   `json:"supplierId"`
	SupplierRating  float64 `json:"supplierRating"`
	SubjectID       int64   `json:"subjectId"`
	SubjectParentID int64   `json:"subjectParentId"`
	ReviewRating    float64 `json:"reviewRating"`
	Feedbacks       int     `json:"feedbacks"`
	Pics            int     `json:"pics"`
	TotalQuantity   int     `json:"totalQuantity"`
	Colors          []struct {
		Name string `json:"name"`
	} `json:"colors"`
	Sizes []struct {
		Name     string `json:"name"`
		OrigName string `json:"origName"`
		Price    struct {
			Basic   int64 `json:"basic"`
			Product int64 `json:"product"`
		} `json:"price"`
	} `json:"sizes"`
}

// Item — плоская модель товара для CSV и JSON. Имена json совпадают с полями
// витрины WB (supplierId, reviewRating, feedbacks, totalQuantity), чтобы
// потребители wb-searchd читали ответ как исходную выдачу.
type Item struct {
	Query          string  `json:"query"`
	Page           int     `json:"page"`
	Position       int     `json:"position"`
	ID             int64   `json:"id"`
	Name           string  `json:"name"`
	Entity         string  `json:"entity"`
	Brand          string  `json:"brand"`
	BrandID        int64   `json:"brandId"`
	Supplier       string  `json:"supplier"`
	SupplierID     int64   `json:"supplierId"`
	SupplierRating float64 `json:"supplierRating"`
	SubjectID      int64   `json:"subjectId"`
	Rating         float64 `json:"reviewRating"`
	Feedbacks      int     `json:"feedbacks"`
	Price          float64 `json:"price"`     // рубли, со скидкой
	BasePrice      float64 `json:"basePrice"` // рубли, без скидки
	DiscountPct    int     `json:"discountPct"`
	Quantity       int     `json:"totalQuantity"`
	Pics           int     `json:"pics"`
	Colors         string  `json:"colors"`
	Sizes          string  `json:"sizes"`
	URL            string  `json:"url"`
}

// ToItem разворачивает товар в плоскую запись: цена берётся с первого размера
// с ненулевой ценой (витрина отдаёт копейки), размеры и цвета склеиваются.
func (p Product) ToItem(query string, page, pos int) Item {
	it := Item{
		Query:          query,
		Page:           page,
		Position:       pos,
		ID:             p.ID,
		Name:           p.Name,
		Entity:         p.Entity,
		Brand:          p.Brand,
		BrandID:        p.BrandID,
		Supplier:       p.Supplier,
		SupplierID:     p.SupplierID,
		SupplierRating: p.SupplierRating,
		SubjectID:      p.SubjectID,
		Rating:         p.ReviewRating,
		Feedbacks:      p.Feedbacks,
		Quantity:       p.TotalQuantity,
		Pics:           p.Pics,
		URL:            fmt.Sprintf("https://www.wildberries.ru/catalog/%d/detail.aspx", p.ID),
	}

	colors := make([]string, 0, len(p.Colors))
	for _, c := range p.Colors {
		colors = append(colors, c.Name)
	}
	it.Colors = strings.Join(colors, ", ")

	sizes := make([]string, 0, len(p.Sizes))
	for _, s := range p.Sizes {
		name := s.OrigName
		if name == "" {
			name = s.Name
		}
		if name != "" && name != "0" {
			sizes = append(sizes, name)
		}
		if it.Price == 0 && s.Price.Product > 0 {
			it.Price = float64(s.Price.Product) / 100
			it.BasePrice = float64(s.Price.Basic) / 100
		}
	}
	it.Sizes = strings.Join(sizes, ", ")

	if it.BasePrice > 0 {
		it.DiscountPct = int((1 - it.Price/it.BasePrice) * 100)
	}
	return it
}

// Session — запущенный браузер с одной вкладкой витрины.
type Session struct {
	launcher *launcher.Launcher
	browser  *rod.Browser
	page     *rod.Page
	// Logf получает ход поиска (сессия, страницы, остановка). nil = молча.
	Logf func(format string, args ...any)
}

// Open запускает браузер с обычным окном. Путь к бинарнику можно задать
// переменной WB_BROWSER_BIN (в Docker это системный Chromium), иначе go-rod
// скачает свой в ~/.cache/rod при первом запуске.
func Open() (*Session, error) {
	l := launcher.New().Headless(false)
	if bin := os.Getenv("WB_BROWSER_BIN"); bin != "" {
		l = l.Bin(bin)
	}
	u, err := l.Launch()
	if err != nil {
		return nil, fmt.Errorf("запуск браузера: %w", err)
	}
	browser := rod.New().ControlURL(u)
	if err := browser.Connect(); err != nil {
		l.Kill()
		l.Cleanup()
		return nil, fmt.Errorf("подключение к браузеру: %w", err)
	}
	page, err := stealth.Page(browser)
	if err != nil {
		_ = browser.Close()
		l.Kill()
		l.Cleanup()
		return nil, fmt.Errorf("вкладка: %w", err)
	}
	return &Session{launcher: l, browser: browser, page: page}, nil
}

// Close закрывает браузер и удаляет его временный профиль: у долгоживущего
// wb-searchd иначе на каждый запрос оставался бы каталог в /tmp.
func (s *Session) Close() {
	_ = s.browser.Close()
	s.launcher.Kill()
	s.launcher.Cleanup()
}

func (s *Session) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// WaitToken открывает поиск как обычный пользователь и ждёт куку x_wbaas_token.
func (s *Session) WaitToken(ctx context.Context, query string, timeout time.Duration) error {
	page := s.page.Context(ctx)
	if err := page.Navigate("https://www.wildberries.ru/catalog/0/search.aspx?search=" + url.QueryEscape(query)); err != nil {
		return fmt.Errorf("страница поиска: %w", err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cookies, err := page.Cookies(nil)
		if err != nil {
			return fmt.Errorf("cookie: %w", err)
		}
		for _, c := range cookies {
			if c.Name == "x_wbaas_token" && c.Value != "" {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("кука x_wbaas_token не появилась за %s", timeout)
}

func buildURL(query string, page int) string {
	q := url.Values{}
	q.Set("query", query)
	q.Set("resultset", "catalog")
	q.Set("appType", "1")
	q.Set("curr", "rub")
	q.Set("dest", "-1257786")
	q.Set("lang", "ru")
	q.Set("page", strconv.Itoa(page))
	q.Set("sort", "popular")
	q.Set("spp", "30")
	return searchURL + "?" + q.Encode()
}

// SearchPage делает fetch изнутри страницы: куки и токен подставит сам браузер.
func (s *Session) SearchPage(ctx context.Context, query string, pageNum int) ([]Product, error) {
	res, err := s.page.Context(ctx).Eval(`async (url) => {
		const r = await fetch(url, {
			credentials: "include",
			headers: {
				"X-Requested-With": "XMLHttpRequest",
				"X-Spa-Version": "14.30.0",
				"deviceid": "site_bb1d272bbd1d45fa83068bcd4dcaa458",
			},
		});
		return { status: r.status, body: await r.text() };
	}`, buildURL(query, pageNum))
	if err != nil {
		return nil, fmt.Errorf("eval: %w", err)
	}

	status := res.Value.Get("status").Int()
	body := res.Value.Get("body").String()
	if status != 200 {
		return nil, fmt.Errorf("http %d: %.300s", status, body)
	}

	var sr Response
	if err := json.Unmarshal([]byte(body), &sr); err != nil {
		return nil, fmt.Errorf("decode: %w; body: %.300s", err, body)
	}
	// WB отдаёт ошибки с HTTP 200, проверяем тело
	if sr.Error != "" {
		return nil, fmt.Errorf("wb error (code %d): %s", sr.Code, sr.Error)
	}
	return sr.items(), nil
}

// SearchAll проходит страницы 1..pages, убирает дубли и останавливается на
// пустой странице. При ошибке возвращает уже собранное вместе с ошибкой.
func (s *Session) SearchAll(ctx context.Context, query string, pages int) ([]Item, error) {
	seen := make(map[int64]bool)
	var all []Item

	for p := 1; p <= pages; p++ {
		raw, err := s.SearchPage(ctx, query, p)
		if err != nil {
			return all, fmt.Errorf("страница %d: %w", p, err)
		}
		if len(raw) == 0 {
			s.logf("страница %d пустая, стоп", p)
			break
		}

		added := 0
		for i, pr := range raw {
			if seen[pr.ID] {
				continue
			}
			seen[pr.ID] = true
			all = append(all, pr.ToItem(query, p, (p-1)*len(raw)+i+1))
			added++
		}
		s.logf("страница %d: %d товаров (новых %d), всего %d", p, len(raw), added, len(all))

		// пауза, чтобы не словить блок
		if p < pages {
			select {
			case <-ctx.Done():
				return all, ctx.Err()
			case <-time.After(time.Duration(800+rand.Intn(1200)) * time.Millisecond):
			}
		}
	}
	return all, nil
}

// Search выполняет один запрос целиком: запускает браузер, получает сессию,
// листает страницы и закрывает браузер. Ошибка на странице возвращается
// вместе с уже собранными товарами, ошибка сессии — без товаров.
func Search(ctx context.Context, query string, pages int, logf func(format string, args ...any)) ([]Item, error) {
	s, err := Open()
	if err != nil {
		return nil, err
	}
	defer s.Close()
	s.Logf = logf

	if err := s.WaitToken(ctx, query, tokenTimeout); err != nil {
		return nil, err
	}
	s.logf("токен получен")
	return s.SearchAll(ctx, query, pages)
}
