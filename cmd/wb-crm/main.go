// wb-crm читает продавцов (вывод wb-sellers: NDJSON или CSV) и заливает их в
// Twenty CRM как сделки с источником WB: компания, контакт, заметка-разбор.
// Браузера здесь нет — на вход идут уже собранные продавцы, поэтому команду
// можно гонять где угодно.
//
//	wb-sellers -q "кроссовки" -enrich -out out/sellers.csv   # у себя, с браузером
//	wb-crm -in out/sellers.csv                               # залить CSV в CRM
//	wb-sellers -ids ids.txt -enrich -format ndjson -out - | wb-crm
//
// Ключи из окружения: TWENTY_API_URL, TWENTY_API_KEY.
package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/OkaneKatsuro/go-wb/internal/crm"
	"github.com/OkaneKatsuro/go-wb/internal/wb"
)

// record — одна запись продавца. Поля совпадают с NDJSON от wb-sellers
// (internal/collect/ndjson.go); из CSV заполняются те же поля по заголовку.
type record struct {
	SupplierID int            `json:"supplierId"`
	Name       string         `json:"supplierName"`
	FullName   string         `json:"supplierFullName"`
	INN        string         `json:"inn"`
	Trademark  string         `json:"trademark"`
	Query      string         `json:"sourceQuery"`
	SellerType string         `json:"sellerType"`
	NotFound   bool           `json:"notFound"`
	Enrich     *wb.Enrichment `json:"enrich"`
}

func main() { os.Exit(realMain()) }

func realMain() int {
	in := flag.String("in", "-", "файл NDJSON/CSV или «-» для stdin")
	format := flag.String("format", "auto", "формат входа: auto (по расширению), ndjson, csv")
	rps := flag.Float64("rps", 2, "запросов в секунду к CRM")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	base := os.Getenv("TWENTY_API_URL")
	key := os.Getenv("TWENTY_API_KEY")
	if base == "" || key == "" {
		slog.Error("нужны TWENTY_API_URL и TWENTY_API_KEY")
		return 2
	}

	src := os.Stdin
	name := ""
	if *in != "-" {
		f, err := os.Open(*in)
		if err != nil {
			slog.Error("открыть вход", "err", err)
			return 1
		}
		defer f.Close()
		src = f
		name = *in
	}

	kind := strings.ToLower(*format)
	if kind == "auto" {
		if strings.HasSuffix(strings.ToLower(name), ".csv") {
			kind = "csv"
		} else {
			kind = "ndjson"
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := crm.New(base, key, *rps)
	var created, skipped, failed, seen int
	handle := func(rec record) {
		seen++
		lead, ok := toLead(rec)
		if !ok {
			skipped++
			return
		}
		res, err := client.Import(ctx, lead)
		if err != nil {
			slog.Warn("лид не залит", "inn", lead.INN, "err", err)
			failed++
			return
		}
		if res.Created {
			created++
		} else {
			skipped++
		}
	}

	var readErr error
	if kind == "csv" {
		readErr = readCSV(src, func(rec record) bool { handle(rec); return ctx.Err() == nil })
	} else {
		readErr = readNDJSON(src, func(rec record) bool { handle(rec); return ctx.Err() == nil })
	}
	if readErr != nil {
		slog.Error("чтение входа", "err", readErr)
		return 1
	}
	slog.Info("готово", "прочитано", seen, "создано", created, "пропущено", skipped, "ошибок", failed)
	if failed > 0 {
		return 1
	}
	return 0
}

// readNDJSON разбирает поток по строке и зовёт emit; возврат false останавливает.
func readNDJSON(r io.Reader, emit func(record) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			slog.Warn("строка не разобрана", "err", err)
			continue
		}
		if !emit(rec) {
			break
		}
	}
	return sc.Err()
}

// readCSV читает CSV от wb-sellers: BOM, заголовок с именами колонок, ячейки
// обезврежены префиксом ' (CSVSafe), телефоны/почты склеены через «; ».
func readCSV(r io.Reader, emit func(record) bool) error {
	br := bufio.NewReader(r)
	if b, err := br.Peek(3); err == nil && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		_, _ = br.Discard(3)
	}
	cr := csv.NewReader(br)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true // данные реестра бывают с кривыми кавычками, не падаем
	header, err := cr.Read()
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return err
	}
	idx := map[string]int{}
	for i, h := range header {
		idx[strings.TrimSpace(h)] = i
	}
	get := func(row []string, col string) string {
		if i, ok := idx[col]; ok && i < len(row) {
			return unCSVSafe(strings.TrimSpace(row[i]))
		}
		return ""
	}
	for {
		row, err := cr.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		rec := record{
			Name:       get(row, "supplierName"),
			FullName:   get(row, "supplierFullName"),
			INN:        get(row, "inn"),
			Trademark:  get(row, "trademark"),
			Query:      get(row, "source_query"),
			SellerType: get(row, "sellerType"),
			NotFound:   strings.EqualFold(get(row, "notFound"), "true"),
		}
		enr := &wb.Enrichment{
			Director:  get(row, "of_director"),
			ShortName: get(row, "of_shortName"),
			FullName:  get(row, "of_fullName"),
			Status:    get(row, "of_status"),
			OkvedName: get(row, "of_okvedName"),
			Address:   get(row, "of_address"),
			Website:   get(row, "of_website"),
			Phones:    splitList(get(row, "of_phones")),
			Emails:    splitList(get(row, "of_emails")),
		}
		if enr.Director != "" || len(enr.Phones) > 0 || len(enr.Emails) > 0 ||
			enr.Website != "" || enr.ShortName != "" || enr.FullName != "" {
			rec.Enrich = enr
		}
		if !emit(rec) {
			return nil
		}
	}
}

// unCSVSafe снимает один ведущий апостроф, которым CSVSafe обезвреживает ячейки,
// начинающиеся с =,+,-,@ (например телефон «+7…» хранится как «'+7…»).
func unCSVSafe(s string) string {
	return strings.TrimPrefix(s, "'")
}

// splitList режет склеенные через «;» значения (телефоны, почты) и чистит пробелы.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ";") {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// toLead превращает запись в лид для CRM. Продавцы без ИНН (физлица C2C, 404 на
// CDN) пропускаются: их нечем дедупить и некого заводить юрлицом.
func toLead(rec record) (crm.Lead, bool) {
	if rec.NotFound || strings.TrimSpace(rec.INN) == "" {
		return crm.Lead{}, false
	}
	lead := crm.Lead{
		INN:       strings.TrimSpace(rec.INN),
		Name:      firstNonEmpty(rec.Name, rec.FullName),
		Trademark: rec.Trademark,
		Query:     rec.Query,
	}
	if e := rec.Enrich; e != nil {
		lead.Director = e.Director
		lead.Phones = e.Phones
		lead.Emails = e.Emails
		lead.Website = e.Website
		lead.Address = firstNonEmpty(e.Address, e.Region)
		lead.Okved = e.OkvedName
		lead.Status = e.Status
		if lead.Name == "" {
			lead.Name = firstNonEmpty(e.ShortName, e.FullName)
		}
	}
	return lead, true
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
