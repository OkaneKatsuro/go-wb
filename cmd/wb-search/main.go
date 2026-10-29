package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/OkaneKatsuro/go-wb/internal/collect"
	"github.com/OkaneKatsuro/go-wb/internal/storefront"
)

// ---------- CSV ----------

// saveCSV пишет товары через общий collect.ExcelWriter: BOM и защита от
// CSV-инъекции в одном месте для всех экспортов проекта.
func saveCSV(path string, items []storefront.Item) (err error) {
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

// run возвращает ошибку вместо log.Fatal: storefront.Search закрывает браузер
// через defer, а после os.Exit отложенные вызовы не выполняются, и процесс
// Chrome остался бы висеть.
func run(query string, pages int, out string) error {
	items, err := storefront.Search(context.Background(), query, pages, func(format string, args ...any) {
		fmt.Printf(format+"\n", args...)
	})
	if err != nil {
		if len(items) == 0 {
			return err
		}
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
