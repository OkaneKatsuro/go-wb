package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := "# ключ OFData\nTEST_WB_KEY=abc123\nexport TEST_WB_QUOTED=\"со пробелом\"\nTEST_WB_PRESET=from-file\nмусор без знака равно\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_WB_PRESET", "from-env") // окружение важнее файла
	t.Setenv("TEST_WB_KEY", "")
	t.Setenv("TEST_WB_QUOTED", "")

	if err := loadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("TEST_WB_KEY"); got != "abc123" {
		t.Fatalf("TEST_WB_KEY = %q", got)
	}
	if got := os.Getenv("TEST_WB_QUOTED"); got != "со пробелом" {
		t.Fatalf("кавычки должны сниматься: %q", got)
	}
	if got := os.Getenv("TEST_WB_PRESET"); got != "from-env" {
		t.Fatalf("переменная окружения не должна перекрываться файлом: %q", got)
	}
	if err := loadDotEnv(filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Fatalf("отсутствие файла — не ошибка: %v", err)
	}
}
