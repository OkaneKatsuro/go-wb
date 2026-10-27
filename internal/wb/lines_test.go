package wb

import (
	"bufio"
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
