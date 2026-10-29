# Серверная часть go-wb: обогащение по готовому списку supplierId и заливка в
# CRM. Браузера (wb-search) здесь НЕТ: путь `-ids` ходит только в открытый CDN
# WB и OFData, поэтому образ лёгкий и влезает даже на тесный диск.
# Сам поиск по витрине (wb-search, go-rod + Chrome) запускается отдельно, не тут.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/wb-sellers ./cmd/wb-sellers \
 && CGO_ENABLED=0 go build -trimpath -o /out/wb-crm ./cmd/wb-crm

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 1001 app
COPY --from=build /out/wb-sellers /usr/local/bin/wb-sellers
COPY --from=build /out/wb-crm /usr/local/bin/wb-crm
USER app
WORKDIR /data
# Команда задаётся при запуске, например:
#   docker compose run --rm import \
#     sh -c 'wb-sellers -ids /data/ids.txt -enrich -enrich-ip -format ndjson -out - | wb-crm'
CMD ["wb-crm", "-h"]
