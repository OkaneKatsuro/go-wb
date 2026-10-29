package crm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type captured struct {
	method, path string
	body         map[string]any
}

// server stubs the Twenty REST API and records the calls. `existing` makes the
// amoId lookup return a deal, so the dedup path can be tested.
func server(t *testing.T, calls *[]captured, existing bool) *httptest.Server {
	t.Helper()
	reply := map[string]string{
		"POST /rest/companies":     "createCompany",
		"POST /rest/people":        "createPerson",
		"POST /rest/opportunities": "createOpportunity",
		"POST /rest/notes":         "createNote",
		"POST /rest/noteTargets":   "createNoteTarget",
	}
	ids := map[string]string{
		"POST /rest/companies":     "co-1",
		"POST /rest/people":        "pe-1",
		"POST /rest/opportunities": "op-1",
		"POST /rest/notes":         "n-1",
		"POST /rest/noteTargets":   "nt-1",
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			_ = json.Unmarshal(raw, &body)
		}
		*calls = append(*calls, captured{method: r.Method, path: r.URL.Path, body: body})
		key := r.Method + " " + r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			switch {
			case strings.HasSuffix(r.URL.Path, "/opportunities") && existing:
				io.WriteString(w, `{"data":{"opportunities":[{"id":"op-old"}]}}`)
			case strings.HasSuffix(r.URL.Path, "/opportunities"):
				io.WriteString(w, `{"data":{"opportunities":[]}}`)
			case strings.HasSuffix(r.URL.Path, "/companies"):
				io.WriteString(w, `{"data":{"companies":[]}}`)
			default:
				io.WriteString(w, `{"data":{"people":[]}}`)
			}
			return
		}
		io.WriteString(w, `{"data":{"`+reply[key]+`":{"id":"`+ids[key]+`"}}}`)
	}))
}

func sampleLead() Lead {
	return Lead{
		INN:       "7203195439",
		Name:      `ООО "РГК"`,
		Director:  "ИВАНОВ ИВАН ИВАНОВИЧ",
		Phones:    []string{"+73452393070"},
		Emails:    []string{"rgk.07@mail.ru"},
		Website:   "http://rgk72.ru",
		Trademark: "РГК",
		Query:     "котельная купить",
		Okved:     "Производство котлов",
		Status:    "Действует",
	}
}

func TestImportCreatesDealWithContactAndNote(t *testing.T) {
	var calls []captured
	srv := server(t, &calls, false)
	defer srv.Close()

	res, err := New(srv.URL, "k", 1000).Import(context.Background(), sampleLead())
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if !res.Created || res.OpportunityID != "op-1" {
		t.Fatalf("ожидал created op-1, получил %+v", res)
	}

	var seq []string
	for _, c := range calls {
		seq = append(seq, c.method+" "+c.path)
	}
	want := []string{
		"GET /rest/opportunities",
		"GET /rest/companies",
		"POST /rest/companies",
		"GET /rest/people",
		"POST /rest/people",
		"POST /rest/opportunities",
		"POST /rest/notes",
		"POST /rest/noteTargets",
	}
	if strings.Join(seq, ",") != strings.Join(want, ",") {
		t.Fatalf("последовательность вызовов:\n got %v\nwant %v", seq, want)
	}

	opp := calls[5].body
	if opp["amoId"] != "wb:7203195439" || opp["stage"] != "NERAZOBRANNOE" {
		t.Errorf("сделка: amoId/stage неверные: %+v", opp)
	}
	if opp["pointOfContactId"] != "pe-1" || opp["companyId"] != "co-1" {
		t.Errorf("сделка не связана с контактом/компанией: %+v", opp)
	}
	person := calls[4].body
	emails, _ := person["emails"].(map[string]any)
	if emails["primaryEmail"] != "rgk.07@mail.ru" {
		t.Errorf("контакт без e-mail: %+v", person)
	}
	phones, _ := person["phones"].(map[string]any)
	if phones["primaryPhoneNumber"] != "+73452393070" {
		t.Errorf("контакт без телефона: %+v", person)
	}
	note := calls[6].body
	v2, _ := note["bodyV2"].(map[string]any)
	md, _ := v2["markdown"].(string)
	if !strings.Contains(md, "WB-продавец") || !strings.Contains(md, "РГК") {
		t.Errorf("заметка без разбора: %q", md)
	}
	if strings.Contains(md, "—") {
		t.Errorf("в заметке длинное тире: %q", md)
	}
}

func TestImportDeduplicatesByAmoId(t *testing.T) {
	var calls []captured
	srv := server(t, &calls, true)
	defer srv.Close()

	res, err := New(srv.URL, "k", 1000).Import(context.Background(), sampleLead())
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.Created || res.OpportunityID != "op-old" {
		t.Fatalf("ожидал существующую op-old без создания, получил %+v", res)
	}
	if len(calls) != 1 || calls[0].method != http.MethodGet {
		t.Fatalf("дедуп должен быть одним GET, получил %d вызовов", len(calls))
	}
}

func TestImportNeedsINN(t *testing.T) {
	_, err := New("http://x", "k", 1000).Import(context.Background(), Lead{Name: "no inn"})
	if err == nil {
		t.Fatal("ожидал ошибку на пустой ИНН")
	}
}
