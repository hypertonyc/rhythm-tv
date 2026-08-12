#!/bin/bash
# Сборка, подпись и установка Tizen-клиента (client/) на телевизор.
# Вызывается задачей "Tizen: Build & Install on TV" из .vscode/tasks.json,
# запускается и руками: ./scripts/tizen-build-install.sh
#
# Пути к тулчейну, адрес телевизора, профиль подписи и DEFAULT_SERVER берутся
# из .env в корне репозитория (шаблон — .env.example).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLIENT="$ROOT/client"

[ -f "$ROOT/.env" ] || { echo 'ERROR: .env not found'; exit 1; }
set -a
# shellcheck disable=SC1091
source "$ROOT/.env"
set +a

[ -n "${TIZEN:-}" ] && [ -n "${TZCLI:-}" ] && [ -n "${SDB:-}" ] && [ -n "${TV:-}" ] \
  && [ -n "${PROFILE:-}" ] && [ -n "${BUILD_DIR:-}" ] \
  || { echo 'ERROR: .env must set TIZEN, TZCLI, SDB, TV, PROFILE, BUILD_DIR'; exit 1; }

BUILD="$CLIENT/$BUILD_DIR"

case "${DEFAULT_SERVER:-}" in
  http://*|https://*) ;;
  *) echo 'ERROR: .env must set DEFAULT_SERVER to a full http(s) URL'; exit 1;;
esac

echo '=== Config ==='
# Адрес сервера запекается в пакет, чтобы не вводить его с пульта.
printf 'window.RTV_CONFIG = { defaultServer: "%s" };\n' "$DEFAULT_SERVER" > "$CLIENT/js/config.js"
echo "DEFAULT_SERVER: $DEFAULT_SERVER"

echo '=== Connect ==='
"$SDB" connect "$TV" >/dev/null 2>&1 || true
case "$("$SDB" devices)" in
  *"$TV"*) ;;
  *) echo "ERROR: TV $TV is not connected"; exit 1;;
esac

echo '=== Clean ==='
rm -rf "$BUILD"

echo '=== Build ==='
"$TZCLI" build -w "$CLIENT" -s "$PROFILE"

echo '=== Package & sign ==='
"$TZCLI" pack -w "$CLIENT" -s "$PROFILE"

WGT="$(find "$BUILD" -maxdepth 1 -type f -name '*.wgt' -print -quit)"
[ -n "$WGT" ] || { echo 'ERROR: WGT not found'; exit 1; }

# Файл, не вписанный в files: в tizen_web_project.yaml, молча не попадёт в пакет,
# и приложение сломается уже на телевизоре — поэтому проверяем содержимое .wgt.
LIST="$(unzip -l "$WGT")"
case "$LIST" in
  *signature1.xml*) ;;
  *) echo "ERROR: $WGT is not signed - check PROFILE=$PROFILE"; exit 1;;
esac
case "$LIST" in
  *js/app.js*) ;;
  *) echo 'ERROR: js/app.js missing from package - check the files: list in client/tizen_web_project.yaml'; exit 1;;
esac
case "$LIST" in
  *js/config.js*) ;;
  *) echo 'ERROR: js/config.js missing from package - check the files: list in client/tizen_web_project.yaml'; exit 1;;
esac
echo "WGT: $WGT"

echo '=== Install ==='
"$TIZEN" install -s "$TV" -n "$(basename "$WGT")" -- "$(dirname "$WGT")"

echo '=== DONE ==='
