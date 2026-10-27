package wb

import (
	"bufio"
	"io"
	"os"
	"strconv"
	"strings"
)

// maxLineBytes — предел длины одной строки ввода. Строка длиннее — не id и не
// запрос, а мусор; ParseLines вернёт bufio.ErrTooLong вместо тихого обрыва.
const maxLineBytes = 1 << 20

// ReadLines возвращает значимые строки текстового файла: обрезанные по краям,
// непустые, без строк, начинающихся с #.
func ReadLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	lines, err := ParseLines(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return lines, err
}

// ParseLines — это ReadLines поверх любого reader (файл, поле HTTP-формы, строка).
func ParseLines(r io.Reader) ([]string, error) {
	var res []string
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, maxLineBytes)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l != "" && !strings.HasPrefix(l, "#") {
			res = append(res, l)
		}
	}
	return res, sc.Err()
}

// ParseIDs превращает строки в supplierId. Строка может быть голым числом или
// ссылкой на продавца вида https://www.wildberries.ru/seller/12345?sort=popular.
// Строки, которые не то и не другое, пропускаются.
func ParseIDs(lines []string) []int {
	ids := make([]int, 0, len(lines))
	for _, l := range lines {
		if id := parseID(l); id > 0 {
			ids = append(ids, id)
		}
	}
	return ids
}

func parseID(s string) int {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimRight(s, "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	id, err := strconv.Atoi(s)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}
