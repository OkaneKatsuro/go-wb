package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/stealth"

	"github.com/OkaneKatsuro/go-wb/internal/collect"
)

const searchURL = "https://www.wildberries.ru/__internal/u-search/exactmatch/ru/common/v18/search"

// ---------- Модель ответа: только нужные поля ----------

type searchResponse struct {
	Error    string    `json:"error"`
	Code     int       `json:"code"`
	Products []product `json:"products"`
	Data     *struct {
		Products []product `json:"products"`
	} `json:"data"`
}

func (r searchResponse) items() []product {
	if len(r.Products) > 0 {
		return r.Products
	}
	if r.Data != nil {
		return r.Data.Products
	}
	return nil
}

type product struct {
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

// Item: чистая модель для дальнейшей работы
type Item struct {
	Query          string
	Page           int
	Position       int
	ID             int64
	Name           string
	Entity         string
	Brand          string
	BrandID        int64
	Supplier       string
	SupplierID     int64
	SupplierRating float64
	SubjectID      int64
	Rating         float64
	Feedbacks      int
	Price          float64 // рубли, со скидкой
	BasePrice      float64 // рубли, без скидки
	DiscountPct    int
	Quantity       int
	Pics           int
	Colors         string
	Sizes          string
	URL            string
}

func (p product) toItem(query string, page, pos int) Item {
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
		// цена в копейках, берём первый размер с ненулевой ценой
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

// ---------- Браузер и токен ----------

// openBrowser запускает браузер с обычным окном. Путь к бинарнику можно задать
// переменной WB_BROWSER_BIN (в Docker это системный Chromium), иначе go-rod
// скачает свой в ~/.cache/rod при первом запуске.
func openBrowser() (*rod.Browser, *rod.Page) {
	l := launcher.New().Headless(false)
	if bin := os.Getenv("WB_BROWSER_BIN"); bin != "" {
		l = l.Bin(bin)
	}
	u := l.MustLaunch()
	browser := rod.New().ControlURL(u).MustConnect()
	page := stealth.MustPage(browser)
	return browser, page
}

// waitToken открывает поиск как обычный пользователь и ждёт куку x_wbaas_token
func waitToken(page *rod.Page, query string, timeout time.Duration) error {
	page.MustNavigate("https://www.wildberries.ru/catalog/0/search.aspx?search=" + url.QueryEscape(query))

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, c := range page.MustCookies() {
			if c.Name == "x_wbaas_token" && c.Value != "" {
				return nil
			}
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("кука x_wbaas_token не появилась за %s", timeout)
}

// ---------- Поиск ----------

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

// searchPage делает fetch изнутри страницы: куки и токен подставит сам браузер
func searchPage(page *rod.Page, query string, pageNum int) ([]product, error) {
	res, err := page.Eval(`async (url) => {
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

	var sr searchResponse
	if err := json.Unmarshal([]byte(body), &sr); err != nil {
		return nil, fmt.Errorf("decode: %w; body: %.300s", err, body)
	}
	// WB отдаёт ошибки с HTTP 200, проверяем тело
	if sr.Error != "" {
		return nil, fmt.Errorf("wb error (code %d): %s", sr.Code, sr.Error)
	}
	return sr.items(), nil
}

// searchAll проходит страницы 1..pages, убирает дубли, останавливается на пустой странице
func searchAll(page *rod.Page, query string, pages int) ([]Item, error) {
	seen := make(map[int64]bool)
	var all []Item

	for p := 1; p <= pages; p++ {
		raw, err := searchPage(page, query, p)
		if err != nil {
			return all, fmt.Errorf("страница %d: %w", p, err)
		}
		if len(raw) == 0 {
			fmt.Printf("страница %d пустая, стоп\n", p)
			break
		}

		added := 0
		for i, pr := range raw {
			if seen[pr.ID] {
				continue
			}
			seen[pr.ID] = true
			all = append(all, pr.toItem(query, p, (p-1)*len(raw)+i+1))
			added++
		}
		fmt.Printf("страница %d: %d товаров (новых %d), всего %d\n", p, len(raw), added, len(all))

		// пауза, чтобы не словить блок
		if p < pages {
			time.Sleep(time.Duration(800+rand.Intn(1200)) * time.Millisecond)
		}
	}
	return all, nil
}

// ---------- CSV ----------

// saveCSV пишет товары через общий collect.ExcelWriter: BOM и защита от
// CSV-инъекции в одном месте для всех экспортов проекта.
func saveCSV(path string, items []Item) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		// Ошибка Close — это недописанный файл, её нельзя терять.
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()

	w, err := collect.NewExcelWriter(f)
	if err != nil {
		return err
	}
	if err := w.Write([]string{
		"query", "page", "position", "id", "name", "entity", "brand", "brand_id",
		"supplier", "supplier_id", "supplier_rating", "subject_id",
		"rating", "feedbacks", "price", "base_price", "discount_pct",
		"quantity", "pics", "colors", "sizes", "url",
	}); err != nil {
		return err
	}

	ff := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	for _, it := range items {
		row := []string{
			it.Query,
			strconv.Itoa(it.Page),
			strconv.Itoa(it.Position),
			strconv.FormatInt(it.ID, 10),
			it.Name,
			it.Entity,
			it.Brand,
			strconv.FormatInt(it.BrandID, 10),
			it.Supplier,
			strconv.FormatInt(it.SupplierID, 10),
			ff(it.SupplierRating),
			strconv.FormatInt(it.SubjectID, 10),
			ff(it.Rating),
			strconv.Itoa(it.Feedbacks),
			ff(it.Price),
			ff(it.BasePrice),
			strconv.Itoa(it.DiscountPct),
			strconv.Itoa(it.Quantity),
			strconv.Itoa(it.Pics),
			it.Colors,
			it.Sizes,
			it.URL,
		}
		if err := w.Write(row); err != nil {
			return err
		}
	}
	return w.Flush()
}

// ---------- main ----------

func main() {
	query := flag.String("q", "куртка зимняя женская", "строка поиска")
	pages := flag.Int("pages", 1, "сколько страниц выкачать")
	out := flag.String("out", "", "путь к CSV (по умолчанию wb_<время>.csv)")
	flag.Parse()

	if *pages < 1 {
		log.Fatal("pages должно быть >= 1")
	}
	if *out == "" {
		*out = fmt.Sprintf("wb_%s.csv", time.Now().Format("20060102_150405"))
	}
	if err := run(*query, *pages, *out); err != nil {
		log.Fatal(err)
	}
}

// run возвращает ошибку вместо log.Fatal, чтобы defer успел закрыть браузер:
// после os.Exit отложенные вызовы не выполняются, и процесс Chrome остался бы висеть.
func run(query string, pages int, out string) error {
	browser, page := openBrowser()
	defer browser.MustClose()

	if err := waitToken(page, query, 30*time.Second); err != nil {
		return err
	}
	fmt.Println("токен получен")

	items, err := searchAll(page, query, pages)
	if err != nil {
		// сохраняем то, что успели собрать
		log.Println("ошибка:", err)
	}
	if len(items) == 0 {
		return errors.New("ничего не найдено")
	}

	if err := saveCSV(out, items); err != nil {
		return fmt.Errorf("csv: %w", err)
	}
	fmt.Printf("сохранено %d товаров в %s\n", len(items), out)
	return nil
}
