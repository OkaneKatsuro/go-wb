#!/bin/sh
# Виртуальный экран для wb-search: Chromium запускается с обычным окном, а рисует
# в Xvfb. Поднимаем X-сервер, ждём его сокет и передаём управление команде.
# PID 1 в контейнере — dumb-init: он подбирает процессы Chromium и пробрасывает
# сигналы, а Xvfb гаснет вместе с контейнером.
set -eu

: "${DISPLAY:=:99}"
export DISPLAY
num=${DISPLAY#:}

Xvfb "$DISPLAY" -screen 0 1920x1080x24 -nolisten tcp -noreset >/dev/null 2>&1 &
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
