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
type Cache struct {
	path string
	mu   sync.RWMutex
	m    map[int]*wb.Supplier
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

// Save пишет весь кэш атомарно: сначала во временный файл, потом rename,
// чтобы падение посреди записи не оставило наполовину записанный кэш.
func (c *Cache) Save() error {
	c.mu.RLock()
	b, err := json.MarshalIndent(c.m, "", " ")
	c.mu.RUnlock()
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}
