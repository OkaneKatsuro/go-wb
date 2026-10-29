# go-wb в Docker. Два образа из одного файла, выбираются через --target:
#
#   slim (по умолчанию): wb-sellers + wb-crm без браузера. Путь `-ids` ходит только
#     в открытый CDN WB и OFData, образ около 30 МБ.
#   browser: то же плюс wb-search и wb-searchd с Chromium и виртуальным экраном
#     Xvfb. Браузер открывает обычное окно, как на ноутбуке, только экран
#     виртуальный, поэтому код поиска не меняется. Песочницы Chromium в контейнере нет (go-rod
#     добавляет --no-sandbox, увидев контейнер), поэтому ключи этому образу в compose
#     не передаются: поиск отдаёт id продавцов, юрданные и CRM идут вторым шагом в slim.
#
# Сборка под сервер x86_64 с Mac на Apple Silicon: Go кросс-компилируется,
# эмулируется только установка пакетов в образ.
#   docker buildx build --platform linux/amd64 --target browser -t go-wb-browser:latest --load .
#   docker save go-wb-browser:latest | ssh crm docker load
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -o /out/ ./cmd/wb-sellers ./cmd/wb-crm ./cmd/wb-search ./cmd/wb-searchd

# --- browser: Chromium + Xvfb + все три команды -------------------------------
FROM debian:trixie-slim AS browser
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      chromium xvfb dumb-init ca-certificates tzdata \
      fonts-liberation fonts-dejavu-core \
 && rm -rf /var/lib/apt/lists/* \
 && useradd -m -u 1001 app \
 && mkdir -p /tmp/.X11-unix && chmod 1777 /tmp/.X11-unix
COPY --from=build /out/wb-sellers /out/wb-crm /out/wb-search /out/wb-searchd /usr/local/bin/
COPY --chmod=755 docker/entrypoint.sh /usr/local/bin/entrypoint.sh
# WB_BROWSER_BIN указывает wb-search на системный Chromium: без него go-rod
# скачивает свой браузер при каждом запуске контейнера. TZ и DISPLAY можно
# переопределить в compose.
ENV DISPLAY=:99 \
    TZ=Europe/Moscow \
    WB_BROWSER_BIN=/usr/bin/chromium
USER app
WORKDIR /data
ENTRYPOINT ["dumb-init", "--", "entrypoint.sh"]
# Команда задаётся при запуске, см. docker-compose.yml.
CMD ["wb-search", "-h"]

# --- slim: без браузера, последний этап = цель по умолчанию ---------------------
FROM alpine:3.20 AS slim
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 1001 app
COPY --from=build /out/wb-sellers /out/wb-crm /usr/local/bin/
USER app
WORKDIR /data
# Команда задаётся при запуске, например:
#   docker compose run --rm import \
#     sh -c 'wb-sellers -ids /data/ids.txt -enrich -enrich-ip -format ndjson -out - | wb-crm'
CMD ["wb-crm", "-h"]
