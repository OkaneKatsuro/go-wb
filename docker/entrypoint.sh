#!/bin/sh
# Виртуальный экран для wb-search: Chromium запускается с обычным окном, а рисует
# в Xvfb. Поднимаем X-сервер, ждём его сокет и передаём управление команде.
# PID 1 в контейнере — dumb-init: он подбирает процессы Chromium и пробрасывает
# сигналы, а Xvfb гаснет вместе с контейнером.
set -eu

: "${DISPLAY:=:99}"
export DISPLAY
num=${DISPLAY#:}

# Рестарт контейнера (ребут сервера, restart: unless-stopped) сохраняет /tmp, а с
# ним lock-файл и сокет прошлого Xvfb. Новый Xvfb видит lock и сразу выходит, а
# цикл ниже находит старый сокет и пускает сервис без экрана: так с 20.11.2026
# четыре дня каждый поиск отдавал 502 «Missing X server or $DISPLAY». Наш Xvfb на
# этом дисплее в контейнере единственный, поэтому чужого lock здесь быть не может.
rm -f "/tmp/.X$num-lock" "/tmp/.X11-unix/X$num"

# stderr Xvfb — в лог контейнера: без него причина отказа экрана не видна.
Xvfb "$DISPLAY" -screen 0 1920x1080x24 -nolisten tcp -noreset >/dev/null &
xvfb=$!

i=0
until [ -S "/tmp/.X11-unix/X$num" ]; do
  if ! kill -0 "$xvfb" 2>/dev/null; then
    echo "entrypoint: Xvfb на $DISPLAY не запустился" >&2
    exit 1
  fi
  i=$((i + 1))
  if [ "$i" -gt 100 ]; then
    echo "entrypoint: Xvfb на $DISPLAY не поднялся за 10 с" >&2
    exit 1
  fi
  sleep 0.1
done

exec "$@"
