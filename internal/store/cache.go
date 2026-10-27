// Пакет store сохраняет полученных продавцов между запусками.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/OkaneKatsuro/go-wb/internal/wb"
)

// Cache держит продавцов в памяти и сохраняет их в один JSON-файл.
// Безопасен для конкурентного доступа: collect.Runner зовёт Put сразу из нескольких воркеров.
//
// Объекты, положенные через Put, считаются неизменяемыми: Get отдаёт тот же
// указатель, а Save читает их без копирования. Кто хочет изменить продавца,
// делает копию и кладёт её через Put.
type Cache struct {
	path string
	mu   sync.RWMutex
	m    map[int]*wb.Supplier

	saveMu sync.Mutex // Save целиком: два одновременных Save писали бы в один .tmp
}

// Load читает файл кэша. Отсутствие файла — не ошибка: это пустой кэш.
func Load(path string) (*Cache, error) {
	c := &Cache{path: path, m: map[int]*wb.Supplier{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(b, &c.m); err != nil {
		return nil, fmt.Errorf("кэш %s: %w", path, err)
	}
	return c, nil
}

func (c *Cache) Get(id int) (*wb.Supplier, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.m[id]
	return s, ok
}

// Put кладёт продавца в кэш. После Put объект менять нельзя (см. Cache).
func (c *Cache) Put(s *wb.Supplier) {
	c.mu.Lock()
	c.m[s.ID] = s
	c.mu.Unlock()
}

func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.m)
}

// Save пишет весь кэш атомарно: сначала во временный файл с fsync, потом rename,
// чтобы падение посреди записи не оставило наполовину записанный кэш.
func (c *Cache) Save() error {
	c.saveMu.Lock()
	defer c.saveMu.Unlock()

	c.mu.RLock()
	b, err := json.MarshalIndent(c.m, "", " ")
	c.mu.RUnlock()
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := writeFileSync(tmp, b); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// writeFileSync — os.WriteFile плюс fsync; при ошибке удаляет недописанный файл.
func writeFileSync(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}
