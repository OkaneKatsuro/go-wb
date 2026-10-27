# syntax=docker/dockerfile:1

# Этап 1: собрать статический бинарник. CGO_ENABLED=0 означает отсутствие
# зависимости от libc, поэтому финальному образу не нужно ничего, кроме самого бинарника.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wb-server ./cmd/wb-server

# Этап 2: в distroless нет ни shell, ни пакетного менеджера, только CA-сертификаты и tzdata.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/wb-server /wb-server
COPY windows-chrome152.json /profiles/wb-search.json
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/wb-server", "-addr", ":8080", "-data", "/data", "-profile", "/profiles/wb-search.json"]
