package store

import (
	"path/filepath"
	"sync"
	"testing"

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
	c.Put(&wb.Supplier{ID: 8, NotFound: true})
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
