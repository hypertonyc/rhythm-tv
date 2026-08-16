#!/usr/bin/env bash
# Сверка Go-сервера с Node-эталоном: поднимает оба на одном торренте
# и гоняет одинаковые запросы, сравнивая ответы.
#
#   ./scripts/compat-node-vs-go.sh data/tbbt.torrent
#
# Node-эталон живёт в server/legacy и собирается в Docker (node в системе нет).
# Пока эталон в дереве, это главная проверка того, что контракт с телевизором
# не поехал: golden-тесты в go test покрывают чистые функции, а здесь
# сравниваются настоящие HTTP-ответы целиком — статус, заголовки и тело.
#
# Что НЕ сравнивается побайтово и почему: /api/status и снимки сеанса содержат
# живые счётчики (пиры, скорость, id, метки времени), поэтому для них
# сравнивается форма — каждое листовое значение заменяется своим типом
# с сохранением порядка ключей. Это ловит пропавший ключ, null вместо нуля
# и [] вместо null, но не спотыкается о разное число пиров.
set -uo pipefail

TORRENT="${1:-data/tbbt.torrent}"
NODE_PORT="${NODE_PORT:-18000}"
GO_PORT="${GO_PORT:-18001}"
NODE="http://127.0.0.1:${NODE_PORT}"
GO="http://127.0.0.1:${GO_PORT}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1

[ -f "$TORRENT" ] || { echo "нет файла $TORRENT" >&2; exit 1; }
[ -d server/legacy ] || { echo "server/legacy удалён — сверять больше не с чем" >&2; exit 1; }

WORK="$(mktemp -d)"
STORE="$(mktemp -d)"
BIN="$WORK/rtv-server"
PASS=0; FAIL=0

cleanup() {
  [ -n "${GO_PID:-}" ] && kill "$GO_PID" 2>/dev/null
  docker rm -f tms-compat-ref >/dev/null 2>&1
  rm -rf "$WORK" "$STORE"
}
trap cleanup EXIT

echo "== сборка =="
docker build -q -t tms-node:compat server/legacy >/dev/null || exit 1
(cd server && go build -o "$BIN" .) || exit 1

echo "== запуск =="
docker run -d --name tms-compat-ref -p "${NODE_PORT}:8000" \
  -v "$ROOT/$(dirname "$TORRENT"):/data:ro" tms-node:compat "/data/$(basename "$TORRENT")" >/dev/null
PORT="$GO_PORT" TORRENT_STORE="$STORE" "$BIN" "$TORRENT" >"$WORK/go.log" 2>&1 &
GO_PID=$!

for _ in $(seq 1 60); do
  n=$(curl -s -m 2 "$NODE/api/status" | grep -c '"ready":true' || true)
  g=$(curl -s -m 2 "$GO/api/status"   | grep -c '"ready":true' || true)
  [ "$n" = "1" ] && [ "$g" = "1" ] && break
  sleep 1
done
if [ "${n:-0}" != "1" ] || [ "${g:-0}" != "1" ]; then
  echo "серверы не поднялись"
  tail -20 "$WORK/go.log"
  exit 1
fi

# Нормализация. Каждое правило здесь — осознанная уступка, а не заметание
# расхождения под ковёр; новые различия по-прежнему всплывут.
#
#  1. Транспортные заголовки (Date, Connection, Transfer-Encoding) отброшены:
#     они про соединение, а не про контракт.
#  2. От строки статуса берётся только КОД. Пояснение к нему по RFC 9110
#     ненормативно и клиентами не читается, а Go и Node формулируют 416
#     по-разному: «Requested Range Not Satisfiable» против «Range Not
#     Satisfiable». Сравнивать эти строки методически неверно.
#  3. На ответах 416 отбрасывается Content-Length: 0. Node не шлёт его вовсе,
#     Go добавляет сам, и убрать это можно только перехватом соединения.
#     Тело в обоих случаях пустое, /raw читает один ffmpeg, телевизор сюда
#     не ходит. Расхождение принято и записано в CLAUDE.md.
norm_headers() {
  awk '
    /^HTTP\// { code = $2; print "status " code; next }
    { print }
  ' | grep -iE '^(status |content-type|content-length|content-range|cache-control|accept-ranges|access-control)' \
    | tr -d '\r' | tr '[:upper:]' '[:lower:]' \
    | awk '
        { line[NR] = $0; if ($0 == "status 416") is416 = 1 }
        END { for (i = 1; i <= NR; i++) if (!(is416 && line[i] == "content-length: 0")) print line[i] }
      ' | sort
}

shape_of() {
  python3 - "$1" <<'PY'
import json, sys, collections
def shape(v):
    if isinstance(v, dict):  return collections.OrderedDict((k, shape(x)) for k, x in v.items())
    if isinstance(v, list):  return [shape(v[0])] if v else []
    if v is None:            return "null"
    if isinstance(v, bool):  return "bool"
    if isinstance(v, (int, float)): return "number"
    return "string"
print(json.dumps(shape(json.load(open(sys.argv[1]))), indent=1))
PY
}

cmp_full() {
  local name="$1"; shift
  curl -s -D "$WORK/n.h" -o "$WORK/n.b" --max-time 20 "$@" "$NODE${URLPATH}" 2>/dev/null
  curl -s -D "$WORK/g.h" -o "$WORK/g.b" --max-time 20 "$@" "$GO${URLPATH}" 2>/dev/null
  norm_headers <"$WORK/n.h" >"$WORK/n.hn"; norm_headers <"$WORK/g.h" >"$WORK/g.hn"
  if diff -q "$WORK/n.hn" "$WORK/g.hn" >/dev/null && cmp -s "$WORK/n.b" "$WORK/g.b"; then
    PASS=$((PASS+1)); printf '  ok   %s\n' "$name"
  else
    FAIL=$((FAIL+1)); printf '  FAIL %s\n' "$name"
    diff "$WORK/n.hn" "$WORK/g.hn" | sed 's/^/       hdr /' | head -12
    cmp -s "$WORK/n.b" "$WORK/g.b" || {
      printf '       body node: %s\n' "$(head -c 200 "$WORK/n.b")"
      printf '       body go:   %s\n' "$(head -c 200 "$WORK/g.b")"
    }
  fi
}

cmp_headers() {
  local name="$1"; shift
  curl -s -D "$WORK/n.h" -o /dev/null --max-time 20 "$@" "$NODE${URLPATH}" 2>/dev/null
  curl -s -D "$WORK/g.h" -o /dev/null --max-time 20 "$@" "$GO${URLPATH}" 2>/dev/null
  norm_headers <"$WORK/n.h" >"$WORK/n.hn"; norm_headers <"$WORK/g.h" >"$WORK/g.hn"
  if diff -q "$WORK/n.hn" "$WORK/g.hn" >/dev/null; then
    PASS=$((PASS+1)); printf '  ok   %s\n' "$name"
  else
    FAIL=$((FAIL+1)); printf '  FAIL %s\n' "$name"
    diff "$WORK/n.hn" "$WORK/g.hn" | sed 's/^/       /' | head -12
  fi
}

# cmp_shell — сверка «/» без тела.
#
# Встроенный веб-клиент СОЗНАТЕЛЬНО разошёлся с эталоном: в него добавлена
# библиотека торрентов (загрузка с телефона и выбор активного), которой
# в Node нет и не будет. Сравнивать тело больше не с чем, поэтому
# от корневой страницы проверяется то, что обязано совпадать по-прежнему:
# код ответа и заголовки, кроме длины. Телевизор сюда не ходит вовсе —
# «/» читает только браузер.
cmp_shell() {
  local name="$1"; shift
  curl -s -D "$WORK/n.h" -o /dev/null --max-time 20 "$@" "$NODE${URLPATH}" 2>/dev/null
  curl -s -D "$WORK/g.h" -o /dev/null --max-time 20 "$@" "$GO${URLPATH}" 2>/dev/null
  norm_headers <"$WORK/n.h" | grep -v '^content-length' >"$WORK/n.hn"
  norm_headers <"$WORK/g.h" | grep -v '^content-length' >"$WORK/g.hn"
  if diff -q "$WORK/n.hn" "$WORK/g.hn" >/dev/null; then
    PASS=$((PASS+1)); printf '  ok   %s\n' "$name"
  else
    FAIL=$((FAIL+1)); printf '  FAIL %s\n' "$name"
    diff "$WORK/n.hn" "$WORK/g.hn" | sed 's/^/       /' | head -12
  fi
}

cmp_shape() {
  local name="$1"
  curl -s --max-time 30 "$NODE${URLPATH}" >"$WORK/n.b" 2>/dev/null
  curl -s --max-time 30 "$GO${URLPATH}" >"$WORK/g.b" 2>/dev/null
  shape_of "$WORK/n.b" >"$WORK/n.shape"; shape_of "$WORK/g.b" >"$WORK/g.shape"
  if diff -q "$WORK/n.shape" "$WORK/g.shape" >/dev/null; then
    PASS=$((PASS+1)); printf '  ok   %s\n' "$name"
  else
    FAIL=$((FAIL+1)); printf '  FAIL %s\n' "$name"
    diff "$WORK/n.shape" "$WORK/g.shape" | sed 's/^/       /' | head -20
  fi
}

echo "== детерминированная поверхность =="
URLPATH=/                      cmp_shell "GET /  (веб-клиент: тело разошлось сознательно)"
URLPATH=/api/files             cmp_full "GET /api/files"
URLPATH=/api/stop              cmp_full "GET /api/stop"
URLPATH=/api/stop              cmp_full "POST /api/stop (метод не проверяется)" -X POST
URLPATH=/nope                  cmp_full "GET /nope -> 404"
URLPATH=/api/files             cmp_full "OPTIONS /api/files -> 204" -X OPTIONS
URLPATH=/raw/0                 cmp_full "OPTIONS /raw/0 -> 204" -X OPTIONS
URLPATH=/api/hls-status/nosuch cmp_full "GET /api/hls-status/<нет такого>"
URLPATH=/hls/abc/index.m3u8    cmp_full "GET /hls/<нет сеанса>/index.m3u8"
URLPATH=/hls/abc/index.M3U8    cmp_full "GET /hls/abc/index.M3U8 (регистр важен)"

echo "== краевые случаи роутинга =="
URLPATH=/api/probe/%31            cmp_full "GET /api/probe/%31 (путь не декодируется)"
URLPATH=//api/files               cmp_full "GET //api/files (путь не чистится)"
URLPATH=/api/files/               cmp_full "GET /api/files/"
URLPATH=/API/FILES                cmp_full "GET /API/FILES"
URLPATH=/api/probe/999999         cmp_full "GET /api/probe/999999 (вне границ)"
URLPATH=/api/start/999999         cmp_full "GET /api/start/999999"
URLPATH=/raw/999999               cmp_full "GET /raw/999999"
URLPATH=/raw/99999999999999999999 cmp_full "GET /raw/<индекс больше 2^64>"

echo "== /raw и Range =="
URLPATH=/raw/0 cmp_headers "HEAD /raw/0" -I
# ВНИМАНИЕ: сюда нельзя добавлять curl-флаг -r. Он задаёт диапазон сам и
# при пустом значении подавляет заголовок Range целиком — проверки тогда
# сравнивают два обычных ответа 200 и проходят всегда. Так уже было.
for rg in "0-1" "500-" "-500" "-0" "-" "0-1,3-4" "999999999999-" "5-2"; do
  URLPATH=/raw/0 cmp_headers "GET /raw/0 Range: bytes=$rg" -H "Range: bytes=$rg"
done

echo "== живая поверхность (сравнение формы) =="
URLPATH=/api/status cmp_shape "GET /api/status"

echo "== разбор реального файла =="
# Требует данных из роя: ffprobe читает файл через /raw. На мёртвой раздаче
# оба сервера одинаково ответят таймаутом — это тоже валидное совпадение.
URLPATH=/api/probe/0 cmp_full "GET /api/probe/0"

echo
echo "итого: $PASS ok, $FAIL FAIL"
[ "$FAIL" -eq 0 ]
