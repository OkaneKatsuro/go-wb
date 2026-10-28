package main

import (
	"bufio"
	"errors"
	"os"
	"strings"
)

// loadDotEnv читает файл вида KEY=VALUE (по одной паре на строку, # — комментарий,
// кавычки вокруг значения снимаются) и кладёт пары в окружение, не перекрывая
// уже заданные переменные. Отсутствие файла — не ошибка. Нужен, чтобы ключ
// OFData лежал в .env рядом с проектом, а не в истории shell.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		if key == "" || os.Getenv(key) != "" {
			continue
		}
		if err := os.Setenv(key, val); err != nil {
			return err
		}
	}
	return sc.Err()
}
