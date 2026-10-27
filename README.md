# wb-sellers

Собирает юридические данные продавцов Wildberries: название, ИНН, ОГРН, КПП, адрес.
Источник данных — публичный CDN WB, который отдаёт JSON по `supplierId`.
Откуда брать сами `supplierId`, решает подключаемый источник: готовый список, поиск по запросам или что-то своё.

Два бинарника на одном ядре:

- `wb-sellers` — CLI: файл со списком на входе, CSV на выходе.
- `wb-server` — веб-интерфейс: вставил список, нажал «Запустить», смотришь прогресс, скачиваешь CSV.

## Структура проекта

```
cmd/wb-sellers/      CLI: флаги, сигналы, вызов конвейера
cmd/wb-server/       HTTP-сервер: точка входа, graceful shutdown
internal/wb/         предметная область: Source, SellerHit, Supplier, HTTP-клиенты
internal/store/      Cache: память + JSON-файл, безопасен для горутин
internal/collect/    конвейер Runner: источник → пул воркеров → CSV
internal/web/        HTTP-обработчики, SSE, страница (html/template + embed)
Dockerfile           multi-stage, distroless, итоговый образ ~17 МБ
```

Зависимости между пакетами направлены строго вниз, циклов нет.
Показаны только прямые рёбра: `cmd/*` и `web` импортируют ещё `store` и `wb`, но это уже следует из схемы.

```mermaid
flowchart TD
    srv["cmd/wb-server"] --> web["internal/web<br/>HTTP, SSE, UI"]
    cli["cmd/wb-sellers"] --> collect
    web --> collect["internal/collect<br/>Runner, CSV"]
    collect --> store["internal/store<br/>Cache"]
    collect --> wb["internal/wb<br/>типы, интерфейс Source, HTTP-клиенты"]
    store --> wb
```

## Как это работает

### 1. Поток данных целиком

Оба бинарника строят `wb.Source`, отдают его в `collect.Runner` и забирают результат.
Runner не знает, откуда пришли id, а источник не знает, что с ними будет дальше.

```mermaid
flowchart TD
    CLI["cmd/wb-sellers<br/>флаги → config → run()"]
    SRV["cmd/wb-server<br/>браузер → POST /api/start"]

    subgraph SOURCE["wb.Source — интерфейс с одним методом Collect(ctx, emit)"]
        IDL["IDList<br/>готовый список id"]
        QS["QuerySearch<br/>поиск WB по запросам"]
        MULTI["Multi<br/>несколько источников подряд"]
    end

    CLI --> MULTI
    MULTI --> IDL
    MULTI --> QS
    SRV --> IDL
    SRV --> QS

    SOURCE -->|"emit(SellerHit) — по одному продавцу"| RUN

    subgraph RUN["collect.Runner.Run(ctx)"]
        direction TB
        H["1. Hits[id] = hit<br/>map убирает дубли"]
        I["2. IDs = отсортированные ключи<br/>порядок обхода map случайный"]
        T["3. todo = id, которых ещё нет в кэше"]
        F["4. fetchAll(ctx, todo)<br/>пул воркеров, схема 2"]
        S["5. Cache.Save()<br/>даже после отмены"]
        H --> I --> T --> F --> S
    end

    RUN --> CACHE["store.Cache<br/>map[int]*Supplier + RWMutex<br/>JSON-файл, атомарная запись .tmp → rename"]
    RUN --> CSV["collect.WriteCSV<br/>BOM + заголовок + строка на каждый id"]
    F -.->|"Get(ctx, id)"| CDN["CDN Wildberries<br/>vol/data/supplier-by-id/ID.json"]
```

### 2. Пул воркеров внутри `fetchAll`

Три роли и два канала. Главная горутина создаёт каналы и ждёт результаты.
Производитель кладёт id в `jobs`. Воркеры читают `jobs`, ходят в сеть и пишут в `results`.

```mermaid
flowchart TB
    subgraph MAIN["Run — главная горутина"]
        direction LR
        M1["создать jobs и results<br/>запустить N воркеров (wg.Go)<br/>запустить производителя"]
        M2["for err := range results<br/>done++, при ошибке Failed++<br/>каждые 200 → Cache.Save()<br/>OnProgress(done, total)"]
        M3["return ctx.Err()"]
        M1 --> M2 --> M3
    end

    subgraph PROD["производитель — go func"]
        direction LR
        P1["for id := range todo"]
        P2{"select"}
        P3["jobs &lt;- id"]
        P4["ctx.Done() → break"]
        P5["close(jobs)<br/>wg.Wait()<br/>close(results)"]
        P1 --> P2
        P2 --> P3 --> P1
        P2 --> P4 --> P5
    end

    subgraph WORK["воркеры × N"]
        direction LR
        W1["for id := range jobs"]
        W2["&lt;-tick.C<br/>общий тикер = лимит rps"]
        W3["Client.Get(ctx, id)<br/>HTTP к CDN, ретраи на 429 и 5xx"]
        W4["Cache.Put(s) при успехе"]
        W5["results &lt;- err"]
        W1 --> W2 --> W3 --> W4 --> W5 --> W1
    end

    M1 -.-> P1
    M1 -.-> W1
    P3 ==>|"jobs: chan int, без буфера"| W1
    W5 ==>|"results: chan error"| M2
```

Почему это не зависает при отмене: производитель видит `ctx.Done()`, перестаёт класть в `jobs` и закрывает его.
Воркеры дочитывают канал до конца и выходят из `range`. `wg.Wait()` отпускает производителя, тот закрывает `results`,
и цикл `range results` в главной горутине завершается сам. Ни один `send` не остаётся без получателя.

### 3. Веб-сервер: один запуск задачи глазами браузера

```mermaid
sequenceDiagram
    autonumber
    participant B as Браузер
    participant S as web.Server
    participant R as collect.Runner
    participant C as CDN

    B->>S: GET /
    S-->>B: страница из html/template (embed)
    B->>S: GET /events (EventSource)
    Note over S: каждые 500 мс сверяет State.Version<br/>и шлёт data: State в JSON, если он изменился

    B->>S: POST /api/start (mode, input, pages)
    alt задача уже выполняется
        S-->>B: 409 Conflict
    else свободно
        S->>S: ParseLines → Source<br/>start(): новый job, ctx с cancel
        S-->>B: 202 Accepted + State
        S->>R: go run(ctx) → Runner.Run(ctx)
        loop каждый id из todo
            R->>C: GET supplier-by-id/ID.json
            C-->>R: 200 JSON или 404
            R->>S: OnProgress → update(job) → Version++
            S-->>B: SSE data: State
        end
        R-->>S: Result → WriteCSVFile, Running = false
        S-->>B: SSE stage = done, rows, sellers
    end

    B->>S: POST /api/cancel
    S->>R: job.cancel() → ctx отменён
    R-->>S: ctx.Err(), кэш сохранён
    S-->>B: SSE stage = cancelled

    B->>S: GET /download
    S-->>B: text/csv из результата и кэша
```

### 4. Состояния задачи

Это поле `State.Stage`, которое видит интерфейс.

```mermaid
stateDiagram-v2
    [*] --> search : POST /api/start
    search --> legal : id собраны
    search --> cancelled : cancel или SIGTERM
    search --> error : источник вернул ошибку
    legal --> done : все id обработаны, CSV записан
    legal --> cancelled : cancel или SIGTERM (кэш сохранён)
    legal --> error : не удалось сохранить кэш
    done --> [*]
    cancelled --> [*]
    error --> [*]
```

### 5. Остановка сервера без потери данных

Порядок важен: сначала гасится задача, потом освобождаются SSE-потоки, и только потом сервер перестаёт принимать соединения.
Иначе `Shutdown` ждал бы вечно открытые `/events`.

```mermaid
flowchart TD
    SIG["Ctrl+C или SIGTERM<br/>(docker stop)"] --> NC["signal.NotifyContext<br/>отменяет корневой ctx"]
    NC --> STOP["app.Stop()"]
    STOP --> S1["job.cancel()<br/>пул воркеров сворачивается"]
    S1 --> S2["ожидание close(job.finished)<br/>Runner уже сохранил кэш"]
    S2 --> S3["close(done)<br/>SSE-обработчики выходят из цикла"]
    S3 --> SH["srv.Shutdown(15 с)<br/>новые соединения не принимаются,<br/>текущие запросы дорабатывают"]
    SH --> SAVE["cache.Save() ещё раз"]
    SAVE --> EXIT["выход с кодом 0"]
```

### 6. Один `ctx` на всё

Контекст создаётся один раз наверху и передаётся вниз первым аргументом через каждую функцию.
Отмена в любой точке доходит до каждого `select`, каждого `sleep` и каждого HTTP-запроса.

```mermaid
flowchart LR
    A["main: signal.NotifyContext<br/>или web.start: context.WithCancel"] --> B["Runner.Run(ctx)"]
    B --> C["Source.Collect(ctx, emit)"]
    B --> D["fetchAll(ctx, todo)"]
    D --> E["Client.Get(ctx, id)"]
    E --> F["http.NewRequestWithContext"]
    E --> G["sleep(ctx, d) между ретраями"]
    C --> H["Searcher.fetchPage(ctx)"]
    H --> F
    H --> G
```

## Запуск

```bash
# CLI: список id или ссылок на продавца, по одному на строку
go run ./cmd/wb-sellers -ids ids.txt -out out/sellers.csv

# Веб-интерфейс на http://localhost:8080
go run ./cmd/wb-server

# Docker
docker build -t wb-server .
docker run --rm -p 8080:8080 -v "$PWD/out:/data" wb-server
```

Файл `ids.txt` принимает голые числа и ссылки вида `https://www.wildberries.ru/seller/12345`.
Строки, начинающиеся с `#`, пропускаются.

Полный список флагов: `go run ./cmd/wb-sellers -h` и `go run ./cmd/wb-server -h`.

### Доступ к серверу

По умолчанию сервер слушает `127.0.0.1:8080`, то есть доступен только с этой машины.
Он принимает запросы с Host `localhost`, loopback-адресов и имён, заданных через `-allowed-hosts`.
Это защищает от CSRF и DNS-rebinding. Чтобы открыть сервер по сети или по домену:

```bash
go run ./cmd/wb-server -addr :8080 -allowed-hosts sellers.local,10.0.0.5:8080
```

Запуск в Docker через `-p 8080:8080` и обращение по `localhost:8080` работает без настройки.

## Разработка

```bash
go test -race ./...     # тесты с детектором гонок
go vet ./...            # статический анализ
gofmt -l .              # должно быть пусто
```

Тесты не ходят в сеть: CDN подменяется `httptest.NewServer`, кэш живёт в `t.TempDir()`.

## Свой источник продавцов

Любой тип с методом `Collect(ctx context.Context, emit func(wb.SellerHit)) error` подходит как `wb.Source`.
В CLI он добавляется в слайс `sources` в `cmd/wb-sellers/main.go`,
в сервере — в замыкание `NewSearch` в `cmd/wb-server/main.go`.
Контракт: вызывать `emit` синхронно и возвращать `ctx.Err()`, как только контекст отменён.
