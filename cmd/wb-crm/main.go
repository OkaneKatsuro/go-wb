// wb-crm читает продавцов в формате NDJSON (вывод wb-sellers) и заливает их в
// Twenty CRM как сделки с источником WB: компания, контакт, заметка-разбор.
// Браузера здесь нет — на вход идут уже собранные продавцы, поэтому команду
// можно гонять на сервере по расписанию.
//
//	wb-sellers -ids ids.txt -enrich -format ndjson -out - | wb-crm
//	wb-crm -in out/sellers.ndjson
//
// Ключи из окружения: TWENTY_API_URL, TWENTY_API_KEY.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/OkaneKatsuro/go-wb/internal/crm"
	"github.com/OkaneKatsuro/go-wb/internal/wb"
)

// record — строка NDJSON от wb-sellers (см. internal/collect/ndjson.go).
type record struct {
	SupplierID  int            `json:"supplierId"`
	Name        string         `json:"supplierName"`
	FullName    string         `json:"supplierFullName"`
	INN         string         `json:"inn"`
	Trademark   string         `json:"trademark"`
	Query       string         `json:"sourceQuery"`
	SellerType  string         `json:"sellerType"`
	Deactivated bool           `json:"deactivated"`
	NotFound    bool           `json:"notFound"`
	Enrich      *wb.Enrichment `json:"enrich"`
}

func main() {
	os.Exit(realMain())
}

func realMain() int {
	in := flag.String("in", "-", "файл NDJSON или «-» для stdin")
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
	if *in != "-" {
		f, err := os.Open(*in)
		if err != nil {
			slog.Error("открыть вход", "err", err)
			return 1
		}
		defer f.Close()
		src = f
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := crm.New(base, key, *rps)
	var created, skipped, failed, seen int
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		if ctx.Err() != nil {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			slog.Warn("строка не разобрана", "err", err)
			failed++
			continue
		}
		seen++
		lead, ok := toLead(rec)
		if !ok {
			skipped++
			continue
		}
		res, err := client.Import(ctx, lead)
		if err != nil {
			slog.Warn("лид не залит", "inn", lead.INN, "err", err)
			failed++
			continue
		}
		if res.Created {
			created++
		} else {
			skipped++
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Error("чтение входа", "err", err)
		return 1
	}
	slog.Info("готово", "прочитано", seen, "создано", created, "пропущено", skipped, "ошибок", failed)
	if failed > 0 {
		return 1
	}
	return 0
}

// toLead превращает запись NDJSON в лид для CRM. Продавцы без ИНН (физлица C2C,
// 404 на CDN) пропускаются: их нечем дедупить и некого заводить юрлицом.
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
