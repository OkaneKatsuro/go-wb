package ofdata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const companyJSON = `{"data":{"ОГРН":"1027700132195","ИНН":"7707083893","КПП":"773601001",
 "НаимПолн":"ПАО СБЕРБАНК","НаимСокр":"ПАО СБЕРБАНК","Статус":{"Код":"001","Наим":"Действует"},
 "ЮрАдрес":{"АдресРФ":"117312, г. Москва, ул. Вавилова, д. 19"},"Регион":{"Код":"77","Наим":"Москва"},
 "ДатаРег":"1991-06-20","ОКВЭД":{"Код":"64.19","Наим":"Денежное посредничество прочее"},
 "ОКОПФ":{"Код":"12247","Наим":"Публичные акционерные общества"},
 "Руковод":[{"ФИО":"Греф Герман Оскарович"}]},"meta":{"status":"ok","balance":100}}`

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
		w.Write([]byte(companyJSON))
	})
	e, err := c.Enrich(context.Background(), "7707083893")
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind != "ЮЛ" || e.ShortName != "ПАО СБЕРБАНК" || e.Status != "Действует" ||
		e.OGRN != "1027700132195" || e.Director != "Греф Герман Оскарович" ||
		e.OkvedCode != "64.19" || e.Address == "" {
		t.Fatalf("неверный маппинг юрлица: %+v", e)
	}
}

func TestEnrichIP(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/entrepreneur" {
			t.Errorf("ИП должен идти в /entrepreneur, а не %s", r.URL.Path)
		}
		w.Write([]byte(ipJSON))
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
}

func TestEnrichNotFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{},"meta":{"status":"ok","balance":100}}`))
	})
	e, err := c.Enrich(context.Background(), "7700000000")
	if err != nil {
		t.Fatalf("не найдено — не ошибка: %v", err)
	}
	if e != nil {
		t.Fatalf("ожидался nil при отсутствии записи, получено %+v", e)
	}
}

func TestEnrichEmptyINN(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("пустой ИНН не должен уходить в сеть")
	})
	if e, err := c.Enrich(context.Background(), ""); err != nil || e != nil {
		t.Fatalf("пустой ИНН: e=%v err=%v", e, err)
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
