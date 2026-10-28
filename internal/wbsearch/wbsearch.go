// Пакет wbsearch подключает cmd/wb-search к конвейеру: по каждому запросу
// запускает его как подпроцесс, читает полученный CSV и отдаёт продавцов
// через wb.QuerySearcher. Сам поиск по витрине остаётся в cmd/wb-search,
// здесь только оркестрация: запуск, остановка по контексту, разбор вывода.
package wbsearch

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/OkaneKatsuro/go-wb/internal/wb"
)

// binName — имя бинарника, который ищется рядом с текущим и в PATH.
const binName = "wb-search"

// ErrNotFound — бинарник wb-search не найден и запустить его через go run нельзя.
var ErrNotFound = errors.New("wb-search не найден: соберите `go build ./cmd/wb-search` или укажите путь флагом -wb-search")

// Runner запускает wb-search. Нулевое значение рабочее: бинарник ищется сам.
type Runner struct {
	// Bin — путь к бинарнику wb-search. Пусто = рядом с текущим исполняемым
	// файлом, затем в PATH, затем `go run ./cmd/wb-search`, если запуск идёт
	// из каталога модуля.
	Bin string
	// Dir — куда класть временные CSV, "" = системный temp.
	Dir string
}

// command строит команду запуска с аргументами wb-search.
func (r *Runner) command(ctx context.Context, args ...string) (*exec.Cmd, error) {
	if r.Bin != "" {
		if _, err := os.Stat(r.Bin); err != nil {
			return nil, fmt.Errorf("wb-search по пути %s: %w", r.Bin, err)
		}
		return exec.CommandContext(ctx, r.Bin, args...), nil
	}
	if exe, err := os.Executable(); err == nil {
		if p := filepath.Join(filepath.Dir(exe), binName); fileExists(p) {
			return exec.CommandContext(ctx, p, args...), nil
		}
	}
	if p, err := exec.LookPath(binName); err == nil {
		return exec.CommandContext(ctx, p, args...), nil
	}
	if fileExists(filepath.Join("cmd", binName, "main.go")) {
		// Запуск из каталога модуля (go run ./cmd/wb-sellers): собираем и
		// запускаем wb-search тем же go.
		return exec.CommandContext(ctx, "go", append([]string{"run", "./cmd/" + binName}, args...)...), nil
	}
	return nil, ErrNotFound
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// Collect запускает wb-search по одному запросу и отдаёт продавцов из его CSV.
// Отмена ctx завершает подпроцесс (SIGTERM, через 5 с SIGKILL).
func (r *Runner) Collect(ctx context.Context, query string, maxPages int, emit func(wb.SellerHit)) error {
	dir := r.Dir
	if dir == "" {
		dir = os.TempDir()
	}
	tmp := filepath.Join(dir, fmt.Sprintf("wb-search-%d.csv", time.Now().UnixNano()))
	defer func() { _ = os.Remove(tmp) }()

	cmd, err := r.command(ctx, "-q", query, "-pages", strconv.Itoa(max(maxPages, 1)), "-out", tmp)
	if err != nil {
		return err
	}
	// По отмене ctx — SIGTERM всей группе процессов (go run плюс wb-search),
	// а не SIGKILL одному родителю: так закрывается и браузер. Если через
	// WaitDelay пайпы ещё открыты (осиротевший потомок), они закрываются силой.
	ownGroup(cmd)
	cmd.Cancel = func() error { return terminate(cmd) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Stderr = os.Stderr // его сообщения об ошибках идут как есть
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("запуск wb-search: %w", err)
	}
	// Читаем в горутине: Wait с WaitDelay сам закроет пайп, если процесс
	// умер, а кто-то из потомков всё ещё держит его открытым.
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			slog.Info("wb-search", "query", query, "msg", sc.Text())
		}
	}()
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("wb-search: %w", err)
	}

	hits, err := wb.ReadHits(tmp)
	if err != nil {
		return fmt.Errorf("результат wb-search: %w", err)
	}
	for _, h := range hits {
		if h.Query == "" {
			h.Query = query
		}
		emit(h)
	}
	return nil
}

// Source — wb.Source поверх Runner: прогоняет запросы через wb.QuerySearch,
// поэтому упавший запрос пропускается с предупреждением, как и у site-mimic.
type Source struct {
	Runner  Runner
	Queries []string
	Pages   int
}

func (s Source) Collect(ctx context.Context, emit func(wb.SellerHit)) error {
	return wb.QuerySearch{Searcher: &s.Runner, Queries: s.Queries, Pages: s.Pages}.Collect(ctx, emit)
}
