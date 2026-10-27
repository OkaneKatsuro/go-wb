package wb

import (
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
		{"garbage skipped", []string{"abc", "-5", "0", ""}, nil},
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
