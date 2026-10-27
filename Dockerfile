# syntax=docker/dockerfile:1

# Этап 1: собрать статический бинарник. CGO_ENABLED=0 означает отсутствие
# зависимости от libc, поэтому финальному образу не нужно ничего, кроме самого бинарника.
# Версия образа совпадает с toolchain в go.mod, чтобы сборка не качала другой Go.
FROM golang:1.26.10-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wb-server ./cmd/wb-server
# Каталог данных создаём здесь, чтобы ниже скопировать его с правами nonroot:
# distroless не даёт shell, а chown в нём сделать нечем.
RUN mkdir -p /out/data

# Этап 2: в distroless нет ни shell, ни пакетного менеджера, только CA-сертификаты и tzdata.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/wb-server /wb-server
COPY --from=build --chown=65532:65532 /out/data /data
COPY windows-chrome152.json /profiles/wb-search.json
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/wb-server", "-addr", ":8080", "-data", "/data", "-profile", "/profiles/wb-search.json"]
