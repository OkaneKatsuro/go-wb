package storefront

import (
	"encoding/json"
	"net/url"
	"testing"
)

func TestToItemPriceSizesColors(t *testing.T) {
	var p Product
	err := json.Unmarshal([]byte(`{
		"id": 42, "name": "Кроссовки", "supplier": "ООО Ромашка", "supplierId": 7,
		"reviewRating": 4.6, "feedbacks": 120, "totalQuantity": 9,
		"colors": [{"name": "черный"}, {"name": "белый"}],
		"sizes": [
			{"name": "0", "origName": "", "price": {"basic": 0, "product": 0}},
			{"name": "41", "origName": "41 RU", "price": {"basic": 900000, "product": 289400}},
			{"name": "42", "origName": "", "price": {"basic": 900000, "product": 289400}}
		]
	}`), &p)
	if err != nil {
		t.Fatal(err)
	}
	it := p.ToItem("кроссовки", 2, 105)

	if it.Query != "кроссовки" || it.Page != 2 || it.Position != 105 {
		t.Errorf("контекст выдачи: %+v", it)
	}
	if it.Price != 2894 || it.BasePrice != 9000 || it.DiscountPct != 67 {
		t.Errorf("цена: price=%v base=%v discount=%d", it.Price, it.BasePrice, it.DiscountPct)
	}
	if it.Sizes != "41 RU, 42" {
		t.Errorf("размеры: %q (нулевой размер должен отбрасываться)", it.Sizes)
	}
	if it.Colors != "черный, белый" {
		t.Errorf("цвета: %q", it.Colors)
	}
	if it.Rating != 4.6 || it.Feedbacks != 120 || it.Quantity != 9 || it.SupplierID != 7 {
		t.Errorf("сигналы продавца: %+v", it)
	}
	if it.URL != "https://www.wildberries.ru/catalog/42/detail.aspx" {
		t.Errorf("url: %s", it.URL)
	}
}

// Потребители wb-searchd (pipeline) читают JSON как исходную выдачу WB.
func TestItemJSONUsesStorefrontFieldNames(t *testing.T) {
	b, err := json.Marshal(Item{SupplierID: 7, Rating: 4.5, Feedbacks: 3, Quantity: 2})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"supplierId", "reviewRating", "feedbacks", "totalQuantity", "supplier", "id"} {
		if _, ok := m[key]; !ok {
			t.Errorf("в JSON нет поля %q: %s", key, b)
		}
	}
}

func TestResponseItemsFallsBackToData(t *testing.T) {
	var r Response
	if err := json.Unmarshal([]byte(`{"data":{"products":[{"id":1}]}}`), &r); err != nil {
		t.Fatal(err)
	}
	if got := r.items(); len(got) != 1 || got[0].ID != 1 {
		t.Errorf("items() = %+v", got)
	}
}

func TestBuildURL(t *testing.T) {
	u, err := url.Parse(buildURL("куртка зимняя", 3))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("query") != "куртка зимняя" || q.Get("page") != "3" || q.Get("resultset") != "catalog" {
		t.Errorf("параметры: %v", q)
	}
}
