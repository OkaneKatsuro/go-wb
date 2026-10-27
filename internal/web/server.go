// Пакет web — браузерный интерфейс: одна страница, за ней исполнитель задач,
// прогресс через Server-Sent Events, скачивание CSV.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OkaneKatsuro/go-wb/internal/collect"
	"github.com/OkaneKatsuro/go-wb/internal/store"
	"github.com/OkaneKatsuro/go-wb/internal/wb"
)

//go:embed templates/*.html
var templates embed.FS

type Config struct {
	DataDir string // куда пишутся CSV-файлы
	Cache   *store.Cache
	Client  *wb.SupplierClient
	Workers int

	// NewSearch строит Source для режима поиска. nil прячет режим поиска в интерфейсе.
	// Это шов для других способов находить продавцов.
	NewSearch func(queries []string, pages int) (wb.Source, error)
}

// State — то, что видит интерфейс; сериализуется в JSON для /api/state и /events.
type State struct {
	ID         int        `json:"id"`
	Running    bool       `json:"running"`
	Stage      string     `json:"stage"` // search | legal | done | cancelled | error — этап
	Message    string     `json:"message"`
	Done       int        `json:"done"`
	Total      int        `json:"total"`
	Sellers    int        `json:"sellers"`
	Rows       int        `json:"rows"`
	Error      string     `json:"error,omitempty"`
	Warning    string     `json:"warning,omitempty"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Version    uint64     `json:"version"` // растёт при каждом изменении; SSE шлёт, только когда он сдвинулся
}

type job struct {
	cancel   context.CancelFunc
	finished chan struct{}   // закрывается, когда run() возвращается
	state    State           // под защитой Server.mu
	result   *collect.Result // заполняется по завершении, под защитой Server.mu
}

type Server struct {
	cfg  Config
	mux  *http.ServeMux
	tmpl *template.Template

	done     chan struct{} // закрывается в Stop: велит SSE-обработчикам выйти
	stopOnce sync.Once

	mu  sync.Mutex
	job *job // текущая или последняя задача, nil до первого запуска
	seq int
}

var errBusy = errors.New("задача уже выполняется")

func New(cfg Config) *Server {
	s := &Server{
		cfg:  cfg,
		mux:  http.NewServeMux(),
		tmpl: template.Must(template.ParseFS(templates, "templates/*.html")),
		done: make(chan struct{}),
	}
	// Шаблоны Go 1.22: метод + путь, "{$}" значит «ровно /».
	s.mux.HandleFunc("GET /{$}", s.handleIndex)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") })
	s.mux.HandleFunc("GET /api/state", s.handleState)
	s.mux.HandleFunc("GET /events", s.handleEvents)
	s.mux.HandleFunc("POST /api/start", s.handleStart)
	s.mux.HandleFunc("POST /api/cancel", s.handleCancel)
	s.mux.HandleFunc("GET /download", s.handleDownload)
	return s
}

// ServeHTTP делает *Server реализацией http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// Stop отменяет текущую задачу, дожидается её и освобождает SSE-соединения,
// чтобы http.Server.Shutdown смог завершиться.
func (s *Server) Stop() {
	s.mu.Lock()
	j := s.job
	s.mu.Unlock()
	if j != nil {
		j.cancel()
		<-j.finished
	}
	s.stopOnce.Do(func() { close(s.done) })
}

func (s *Server) state() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == nil {
		return State{}
	}
	return s.job.state
}

func (s *Server) update(j *job, fn func(st *State)) {
	s.mu.Lock()
	fn(&j.state)
	j.state.Version++
	s.mu.Unlock()
}

func (s *Server) start(src wb.Source) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job != nil && s.job.state.Running {
		return State{}, errBusy
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.seq++
	j := &job{
		cancel:   cancel,
		finished: make(chan struct{}),
		state: State{
			ID: s.seq, Running: true, Stage: string(collect.StageSearch),
			Message: "запуск", StartedAt: time.Now(), Version: 1,
		},
	}
	s.job = j
	go s.run(ctx, cancel, j, src)
	return j.state, nil
}

func (s *Server) run(ctx context.Context, cancel context.CancelFunc, j *job, src wb.Source) {
	defer close(j.finished)
	defer cancel()
	id := j.state.ID // записан до оператора go, дальше не меняется

	runner := &collect.Runner{
		Source: src, Client: s.cfg.Client, Cache: s.cfg.Cache, Workers: s.cfg.Workers,
		OnProgress: func(p collect.Progress) {
			s.update(j, func(st *State) {
				st.Stage, st.Message, st.Done, st.Total = string(p.Stage), p.Message, p.Done, p.Total
				if p.Stage == collect.StageSearch {
					st.Sellers = p.Done
				}
			})
		},
	}
	res, err := runner.Run(ctx)

	rows := 0
	if res != nil && len(res.IDs) > 0 {
		path := filepath.Join(s.cfg.DataDir, fmt.Sprintf("sellers-%d.csv", id))
		if n, werr := collect.WriteCSVFile(path, res, s.cfg.Cache); werr != nil {
			slog.Warn("csv", "path", path, "err", werr)
		} else {
			rows = n
			slog.Info("csv записан", "path", path, "rows", n)
		}
	}

	now := time.Now()
	s.update(j, func(st *State) {
		st.Running = false
		st.FinishedAt = &now
		st.Rows = rows
		if res != nil {
			st.Sellers = len(res.IDs)
			st.Warning = res.Warning
			if res.Failed > 0 {
				st.Warning = strings.TrimSpace(st.Warning + fmt.Sprintf(" Не получены юрданные: %d.", res.Failed))
			}
		}
		switch {
		case errors.Is(err, context.Canceled):
			st.Stage, st.Message = "cancelled", "остановлено"
		case err != nil:
			st.Stage, st.Message, st.Error = "error", "ошибка", err.Error()
		}
		j.result = res
	})
}

func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	data := struct{ HasSearch bool }{HasSearch: s.cfg.NewSearch != nil}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "index.html", data); err != nil {
		slog.Warn("template", "err", err)
	}
}

func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.state())
}

// handleEvents отдаёт State как Server-Sent Events: по сообщению на изменение,
// проверка дважды в секунду. Браузер сам переподключается, если поток оборвётся.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	var sent uint64 = ^uint64(0) // пока ничего не отправлено; форсирует первое сообщение
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		if st := s.state(); st.Version != sent {
			sent = st.Version
			b, _ := json.Marshal(st)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
		select {
		case <-r.Context().Done(): // браузер ушёл
			return
		case <-s.done: // сервер останавливается
			return
		case <-t.C:
		}
	}
}

// maxInputLines ограничивает размер одной задачи, чтобы запрос не раздул память.
const maxInputLines = 50000

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "перекрёстный запрос отклонён", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20) // 8 МБ на тело формы
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	lines, err := wb.ParseLines(strings.NewReader(r.FormValue("input")))
	if err != nil || len(lines) == 0 {
		http.Error(w, "пустой ввод", http.StatusBadRequest)
		return
	}
	if len(lines) > maxInputLines {
		http.Error(w, "слишком много строк на один запуск", http.StatusRequestEntityTooLarge)
		return
	}
	var src wb.Source
	switch r.FormValue("mode") {
	case "ids":
		ids := wb.ParseIDs(lines)
		if len(ids) == 0 {
			http.Error(w, "не нашёл ни одного supplierId", http.StatusBadRequest)
			return
		}
		src = wb.IDList(ids)
	case "queries":
		if s.cfg.NewSearch == nil {
			http.Error(w, "режим поиска выключен", http.StatusBadRequest)
			return
		}
		pages, _ := strconv.Atoi(r.FormValue("pages"))
		switch {
		case pages <= 0:
			pages = 3
		case pages > 50:
			pages = 50
		}
		if src, err = s.cfg.NewSearch(lines, pages); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	default:
		http.Error(w, "неизвестный режим", http.StatusBadRequest)
		return
	}
	st, err := s.start(src)
	if errors.Is(err, errBusy) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusAccepted, st)
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "перекрёстный запрос отклонён", http.StatusForbidden)
		return
	}
	s.mu.Lock()
	j := s.job
	running := j != nil && j.state.Running
	s.mu.Unlock()
	if !running {
		http.Error(w, "нет запущенной задачи", http.StatusConflict)
		return
	}
	j.cancel()
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleDownload(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	var res *collect.Result
	var id int
	if j := s.job; j != nil && !j.state.Running {
		res, id = j.result, j.state.ID
	}
	s.mu.Unlock()
	if res == nil || len(res.IDs) == 0 {
		http.Error(w, "нет готового результата", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="sellers-%d.csv"`, id))
	if _, err := collect.WriteCSV(w, res, s.cfg.Cache); err != nil {
		slog.Warn("csv download", "err", err)
	}
}

// sameOrigin защищает POST-эндпоинты от CSRF. Браузер сам проставляет Origin
// на кросс-доменных запросах, подделать его со страницы нельзя. Нет Origin
// (curl, тот же сайт) — пропускаем; есть и не совпадает с Host — отклоняем.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("json", "err", err)
	}
}
