package wb

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseLines(t *testing.T) {
	in := "  first \n\n# comment\nsecond\n   \n"
	got, err := ParseLines(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"first", "second"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParseLinesTooLongLineIsError(t *testing.T) {
	// Строка длиннее maxLineBytes — не id и не запрос; важно получить ошибку,
	// а не молча оборванный список (bufio.Scanner по умолчанию так и делает).
	in := "1\n" + strings.Repeat("x", maxLineBytes+1) + "\n2\n"
	_, err := ParseLines(strings.NewReader(in))
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("err = %v, want bufio.ErrTooLong", err)
	}
}

func TestReadLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ids.txt")
	if err := os.WriteFile(path, []byte("# заголовок\n10\n\n20\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLines(path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"10", "20"}) {
		t.Fatalf("got %q", got)
	}
	if _, err := ReadLines(filepath.Join(t.TempDir(), "nope.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: err = %v, want ErrNotExist", err)
	}
}

func TestParseIDs(t *testing.T) {
	// Табличный тест: способ Go покрыть много входов одним циклом.
	tests := []struct {
		name string
		in   []string
		want []int
	}{
		{"bare numbers", []string{"1", " 42 "}, []int{1, 42}},
		{"seller url", []string{"https://www.wildberries.ru/seller/12345"}, []int{12345}},
		{"url with query and slash", []string{"https://www.wildberries.ru/seller/777/?sort=popular"}, []int{777}},
		{"url with fragment", []string{"https://www.wildberries.ru/seller/5#top"}, []int{5}},
		{"garbage skipped", []string{"abc", "-5", "0", "", "1.5"}, nil},
		{"mixed", []string{"abc", "9", "wildberries.ru/seller/10"}, []int{9, 10}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseIDs(tc.in); !slices.Equal(got, tc.want) {
				t.Fatalf("ParseIDs(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestExtractHits(t *testing.T) {
	// CSV из wb-search: BOM, кавычки с запятыми и переносом строки внутри поля,
	// строки с разным числом полей, дубли (их схлопывает Runner), имя и запрос.
	csvIn := "\xEF\xBB\xBFquery,page,supplier,supplier_id,url\n" +
		"куртка,1,\"ИП Иванов, И.\",184,https://x\n" +
		"куртка,1,\"Куртка\nс переносом\",184,https://x\n" +
		"куртка,2,Шапка,abc,https://x\n" + // мусор в колонке id пропускается
		"куртка,2,Короткая строка\n" +
		"шарф,2,ООО Шарф,7\n"
	got, err := ExtractHits(strings.NewReader(csvIn))
	if err != nil {
		t.Fatal(err)
	}
	want := []SellerHit{
		{SupplierID: 184, Supplier: "ИП Иванов, И.", Query: "куртка"},
		{SupplierID: 184, Supplier: "Куртка\nс переносом", Query: "куртка"},
		{SupplierID: 7, Supplier: "ООО Шарф", Query: "шарф"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("CSV: got %+v", got)
	}

	// Та же колонка под именем CDN, регистр не важен, других колонок нет.
	got, _ = ExtractHits(strings.NewReader("SupplierId,name\n5,x\n"))
	if !slices.Equal(got, []SellerHit{{SupplierID: 5}}) {
		t.Fatalf("supplierId: got %+v", got)
	}

	// Обычный список: числа, ссылки, комментарии.
	got, err = ExtractHits(strings.NewReader("# список\n1\nhttps://www.wildberries.ru/seller/2?x=1\n\n"))
	if err != nil || !slices.Equal(got, []SellerHit{{SupplierID: 1}, {SupplierID: 2}}) {
		t.Fatalf("список: got %+v err %v", got, err)
	}

	// CSV без колонки id — это не CSV с id: строки идут как список и дают пусто.
	got, _ = ExtractHits(strings.NewReader("a,b\n1,2\n"))
	if len(got) != 0 {
		t.Fatalf("CSV без supplier_id: got %+v", got)
	}
}

func TestReadHitsAndHitList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.csv")
	if err := os.WriteFile(path, []byte("id,supplier_id\n1,10\n2,20\n3,0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hits, err := ReadHits(path)
	if err != nil || len(hits) != 2 {
		t.Fatalf("got %+v err %v", hits, err)
	}
	var ids []int
	if err := HitList(hits).Collect(context.Background(), func(h SellerHit) { ids = append(ids, h.SupplierID) }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []int{10, 20}) {
		t.Fatalf("ids = %v", ids)
	}
}
