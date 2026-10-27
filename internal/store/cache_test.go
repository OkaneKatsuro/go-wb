package store

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/OkaneKatsuro/go-wb/internal/wb"
)

func TestLoadMissingFileIsEmptyCache(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Len() != 0 {
		t.Fatalf("Len = %d, want 0", c.Len())
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	c.Put(&wb.Supplier{ID: 7, Name: "ИП Иванов", OGRN: "3", NotFound: false})
	c.Put(&wb.Supplier{ID: 8, NotFound: true, FetchedAt: time.Now()}) // без даты Load счёл бы пометку ложной
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}

	c2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s, ok := c2.Get(7)
	if !ok || s.Name != "ИП Иванов" || s.OGRN != "3" {
		t.Fatalf("supplier 7 after reload: %+v, ok=%v", s, ok)
	}
	if s, ok := c2.Get(8); !ok || !s.NotFound {
		t.Fatalf("supplier 8 should be a NotFound stub, got %+v", s)
	}
	if _, ok := c2.Get(9); ok {
		t.Fatal("supplier 9 must be absent")
	}
}

// Запуск с -race: именно здесь поймается незащищённая мапа.
func TestConcurrentPut(t *testing.T) {
	c, _ := Load(filepath.Join(t.TempDir(), "cache.json"))
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Go(func() {
			c.Put(&wb.Supplier{ID: i})
			c.Get(i)
		})
	}
	wg.Wait()
	if c.Len() != 100 {
		t.Fatalf("Len = %d, want 100", c.Len())
	}
}

// Несколько Save одновременно (прогресс-сохранение Runner и Save при остановке
// сервера) не должны ломать файл: каждый пишет полный снимок, последний побеждает.
func TestConcurrentSaveKeepsFileValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	c, _ := Load(path)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			c.Put(&wb.Supplier{ID: i})
			if err := c.Save(); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if _, err := Load(path); err != nil {
		t.Fatalf("файл после параллельных Save не читается: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("временный файл не должен оставаться")
	}
}

func TestLoadCorruptFileIsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("повреждённый кэш должен давать ошибку, а не пустой кэш")
	}
}

// Старая версия ставила NotFound всем id от 1000 из-за неверного пути CDN.
// Такие пометки без даты при загрузке выбрасываются, остальное остаётся.
func TestLoadDropsLegacyNotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	legacy := `{"1":{"supplierId":1,"supplierName":"есть"},
	 "2":{"supplierId":2,"notFound":true},
	 "3":{"supplierId":3,"notFound":true,"fetchedAt":"2026-10-27T10:00:00Z"}}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get(2); ok {
		t.Fatal("ложный NotFound без даты должен быть удалён")
	}
	if s, ok := c.Get(3); !ok || !s.NotFound {
		t.Fatal("NotFound с датой остаётся")
	}
	if _, ok := c.Get(1); !ok || c.Len() != 2 {
		t.Fatalf("обычные записи остаются, Len = %d", c.Len())
	}
}
