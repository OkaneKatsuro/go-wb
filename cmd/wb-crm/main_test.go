package main

import (
	"strings"
	"testing"
)

func collectCSV(t *testing.T, body string) []record {
	t.Helper()
	var recs []record
	if err := readCSV(strings.NewReader(body), func(r record) bool {
		recs = append(recs, r)
		return true
	}); err != nil {
		t.Fatalf("readCSV: %v", err)
	}
	return recs
}

func TestReadCSVMapsColumnsAndUnescapes(t *testing.T) {
	// BOM + заголовок wb-sellers; телефон начинается с '+', поэтому CSVSafe в
	// выгрузке пишет его с ведущим апострофом; телефоны склеены через «; ».
	csv := string([]byte{0xEF, 0xBB, 0xBF}) +
		"supplierId,supplierName,supplierFullName,inn,ogrn,kpp,legalAddress,trademark,taxpayerCode,supplierRating,deactivated,notFound,source_query,sellerType,of_kind,of_shortName,of_fullName,of_ogrn,of_status,of_regDate,of_address,of_okvedCode,of_okvedName,of_director,of_phones,of_emails,of_website\r\n" +
		"5,ООО РГК,ООО \"РГК\",7203195439,,,,РГК,,4.8,false,false,котельная,,ЮЛ,ООО РГК,,,Действует,,Тюмень,,Производство котлов,ИВАНОВ ИВАН,'+73452393070; +79044941104,rgk.07@mail.ru,http://rgk72.ru\r\n" +
		"6,Физик,,,,,,,,,false,false,,C2C,,,,,,,,,,,,,\r\n"

	recs := collectCSV(t, csv)
	if len(recs) != 2 {
		t.Fatalf("ожидал 2 записи, получил %d", len(recs))
	}

	lead, ok := toLead(recs[0])
	if !ok {
		t.Fatal("первая запись должна пройти в лид")
	}
	if lead.INN != "7203195439" || lead.Name != "ООО РГК" || lead.Director != "ИВАНОВ ИВАН" {
		t.Errorf("поля лида неверные: %+v", lead)
	}
	if len(lead.Phones) != 2 || lead.Phones[0] != "+73452393070" || lead.Phones[1] != "+79044941104" {
		t.Errorf("телефоны разобраны неверно: %#v", lead.Phones)
	}
	if len(lead.Emails) != 1 || lead.Emails[0] != "rgk.07@mail.ru" {
		t.Errorf("почты разобраны неверно: %#v", lead.Emails)
	}
	if lead.Query != "котельная" || lead.Okved != "Производство котлов" {
		t.Errorf("запрос/оквэд неверные: %+v", lead)
	}

	// Вторая строка — физлицо без ИНН: в CRM не идёт.
	if _, ok := toLead(recs[1]); ok {
		t.Error("продавец без ИНН не должен попадать в лид")
	}
}
