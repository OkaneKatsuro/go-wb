package ofdata

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OkaneKatsuro/go-wb/internal/wb"
)

const companyJSON = `{"data":{"ОГРН":"1027700132195","ИНН":"7707083893","КПП":"773601001",
 "НаимПолн":"ПАО СБЕРБАНК","НаимСокр":"ПАО СБЕРБАНК","Статус":{"Код":"001","Наим":"Действует"},
 "ЮрАдрес":{"АдресРФ":"117312, г. Москва, ул. Вавилова, д. 19"},"Регион":{"Код":"77","Наим":"Москва"},
 "ДатаРег":"1991-06-20","ОКВЭД":{"Код":"64.19","Наим":"Денежное посредничество прочее"},
 "ОКОПФ":{"Код":"12247","Наим":"Публичные акционерные общества"},
 "Контакты":{"Тел":["+74955005550"," 8 800 555 55 50 ",""],"Емэйл":["sberbank@sberbank.ru"],"ВебСайт":"www.sberbank.ru"},
 "Руковод":[{"ФИО":"Греф Герман Оскарович"}]},"meta":{"status":"ok","today_request_count":1}}`

const ipJSON = `{"data":{"ОГРНИП":"323080000016595","ИНН":"544052334508","ФИО":"Хлюстина Анастасия Владимировна",
 "ТипСокр":"ИП","Статус":{"Код":"001","Наим":"Действует"},"Регион":{"Код":"08","Наим":"Калмыкия, республика"},
 "ДатаРег":"2023-08-02","ОКВЭД":{"Код":"47.91.2","Наим":"Торговля розничная в интернете"},
 "ОКОПФ":{"Код":"50102","Наим":"Индивидуальные предприниматели"}},"meta":{"status":"ok","balance":100}}`

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New("test-key", 1000)
	if err != nil {
		t.Fatal(err)
	}
	c.BaseURL = srv.URL
	c.Backoff = time.Millisecond
	return c
}

func TestEnrichCompany(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/company" {
			t.Errorf("юрлицо должно идти в /company, а не %s", r.URL.Path)
		}
		if r.URL.Query().Get("inn") != "7707083893" || r.URL.Query().Get("key") != "test-key" {
			t.Errorf("неверные параметры: %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(companyJSON))
	})
	e, err := c.Enrich(context.Background(), "7707083893")
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind != "ЮЛ" || e.ShortName != "ПАО СБЕРБАНК" || e.Status != "Действует" ||
		e.OGRN != "1027700132195" || e.Director != "Греф Герман Оскарович" ||
		e.OkvedCode != "64.19" || e.Address == "" || e.Source != "ofdata" || e.NotFound {
		t.Fatalf("неверный маппинг юрлица: %+v", e)
	}
	if len(e.Phones) != 2 || e.Phones[0] != "+74955005550" || e.Phones[1] != "8 800 555 55 50" ||
		len(e.Emails) != 1 || e.Website != "www.sberbank.ru" {
		t.Fatalf("контакты: phones=%v emails=%v site=%q", e.Phones, e.Emails, e.Website)
	}
}

func TestEnrichIP(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/entrepreneur" {
			t.Errorf("ИП должен идти в /entrepreneur, а не %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(ipJSON))
	})
	e, err := c.Enrich(context.Background(), "544052334508")
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind != "ИП" || e.FullName != "Хлюстина Анастасия Владимировна" ||
		e.OGRN != "323080000016595" || e.Director != "Хлюстина Анастасия Владимировна" ||
		e.Region != "Калмыкия, республика" {
		t.Fatalf("неверный маппинг ИП: %+v", e)
	}
	if e.Phones != nil || e.Emails != nil || e.Website != "" {
		t.Fatalf("без блока Контакты поля пустые: %+v", e)
	}
}

func TestEnrichNotFoundIsMarked(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"пустой data": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":{},"meta":{"status":"ok","balance":100}}`))
		},
		"404": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
	} {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, h)
			e, err := c.Enrich(context.Background(), "7700000000")
			if err != nil {
				t.Fatalf("не найдено — не ошибка: %v", err)
			}
			if e == nil || !e.NotFound || e.Source != "ofdata" {
				t.Fatalf("ожидалась пометка NotFound, получено %+v", e)
			}
		})
	}
}

func TestEnrichAPIErrorIsNotNotFound(t *testing.T) {
	// Исчерпанный баланс или неверный запрос OFData может прислать и с 200.
	// Это ошибка, а не «записи нет»: иначе она навсегда осела бы в кэше.
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{},"meta":{"status":"error","message":"Недостаточно средств"}}`))
	})
	e, err := c.Enrich(context.Background(), "7700000000")
	if err == nil || !strings.Contains(err.Error(), "Недостаточно средств") {
		t.Fatalf("e=%v err=%v, ожидалась ошибка с текстом API", e, err)
	}
}

func TestEnrichInvalidINNDoesNotHitNetwork(t *testing.T) {
	c := newTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("неверный ИНН не должен уходить в сеть")
	})
	for _, inn := range []string{"", "123", "123456789", "7707O83893"} {
		if e, err := c.Enrich(context.Background(), inn); err == nil || e != nil {
			t.Errorf("ИНН %q: e=%v err=%v, ожидалась ошибка", inn, e, err)
		}
	}
}

func TestEnrichRetriesOn429And5xx(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			w.WriteHeader(http.StatusBadGateway)
		default:
			_, _ = w.Write([]byte(companyJSON))
		}
	})
	if _, err := c.Enrich(context.Background(), "7707083893"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3", calls.Load())
	}
}

func TestEnrichAuthErrorShortCircuits(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	})
	for range 3 {
		if _, err := c.Enrich(context.Background(), "7707083893"); !errors.Is(err, ErrAuth) {
			t.Fatalf("err = %v, want ErrAuth", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("после отклонённого ключа клиент сходил в сеть %d раз, want 1", calls.Load())
	}
	if !errors.Is(ErrAuth, wb.ErrEnrichUnavailable) {
		t.Fatal("ErrAuth должен оборачивать wb.ErrEnrichUnavailable, иначе Runner не остановится")
	}
}

func TestEnrichRejectsHugeBodyWithoutRetry(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"data":{"ОГРН":"` + strings.Repeat("1", maxBody) + `"}}`))
	})
	_, err := c.Enrich(context.Background(), "7707083893")
	if !errors.Is(err, wb.ErrBodyTooLarge) {
		t.Fatalf("err = %v, want ErrBodyTooLarge", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("слишком большой ответ не повторяется: calls = %d", calls.Load())
	}
}

func TestNewRequiresKey(t *testing.T) {
	if _, err := New("", 3); err == nil {
		t.Fatal("без ключа New должен вернуть ошибку")
	}
}

func TestErrorDoesNotLeakKey(t *testing.T) {
	c, err := New("SUPERSECRETKEY", 1000)
	if err != nil {
		t.Fatal(err)
	}
	c.BaseURL = "http://127.0.0.1:1" // никто не слушает → ошибка Do с URL
	c.Backoff = time.Millisecond
	_, err = c.Enrich(context.Background(), "7707083893")
	if err == nil {
		t.Fatal("ожидалась сетевая ошибка")
	}
	if strings.Contains(err.Error(), "SUPERSECRETKEY") {
		t.Fatalf("ключ утёк в ошибку: %v", err)
	}
}
