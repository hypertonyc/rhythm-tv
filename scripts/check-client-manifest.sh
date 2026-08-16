#!/usr/bin/env bash
# Файл, не вписанный в files: в client/tizen_web_project.yaml, не попадёт в .wgt,
# и приложение молча сломается уже на телевизоре (CLAUDE.md).
#
# scripts/tizen-build-install.sh это проверяет, но только для двух захардкоженных
# имён, только после полной сборки и только с подключённым телевизором.
# Здесь то же самое, но в три стороны и без телевизора.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
YAML="$ROOT/client/tizen_web_project.yaml"
GENERATED='js/config.js' # генерируется сборкой, в git его нет
fail=0

# Разбор списка files: — до первого ключа верхнего уровня (excludes:).
listed="$(awk '
  /^files:/                          { f=1; next }
  f && /^[[:space:]]*-[[:space:]]*/  { sub(/^[[:space:]]*-[[:space:]]*/,""); print; next }
  f && NF && !/^[[:space:]]/         { f=0 }
' "$YAML")"
[ -n "$listed" ] || { echo "ОШИБКА: не разобрал files: в $YAML"; exit 1; }

is_listed() { printf '%s\n' "$listed" | grep -qxF "$1"; }

echo '=== 1. каждый ассет клиента вписан в files: ==='
while IFS= read -r rel; do
  case "$rel" in
    # Манифесты и документация в пакет не кладутся — они в excludes:.
    README.md|tizen_web_project.yaml|.tproject|config.xml) continue ;;
    Debug/*|Release/*)                                     continue ;;
  esac
  is_listed "$rel" || { echo "  ОТСУТСТВУЕТ: client/$rel не вписан в files:"; fail=1; }
done < <(cd "$ROOT" && git ls-files client | sed 's|^client/||')

echo '=== 2. каждая запись files: существует на диске ==='
while IFS= read -r rel; do
  [ -n "$rel" ] || continue
  [ "$rel" = "$GENERATED" ] && continue
  [ -e "$ROOT/client/$rel" ] || { echo "  УСТАРЕЛО: files: ссылается на несуществующий $rel"; fail=1; }
done <<< "$listed"

echo '=== 3. всё, что грузит index.html, вписано в files: ==='
# Самая полезная из трёх проверок: реалистичный сценарий — добавили js/parser.js,
# подключили в html, забыли YAML. Первые две этого не поймают.
while IFS= read -r rel; do
  case "$rel" in '$'*|http*|//*) continue ;; esac # $WEBAPIS/... подставляет прошивка
  is_listed "$rel" || { echo "  ОТСУТСТВУЕТ: index.html грузит $rel, но его нет в files:"; fail=1; }
done < <(grep -oE '(src|href)="[^"]+"' "$ROOT/client/index.html" | sed 's/.*="//; s/"$//')

[ "$fail" -eq 0 ] && echo 'OK'
exit "$fail"
