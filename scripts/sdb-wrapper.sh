#!/bin/bash
# RHYTHM-TV SDB WRAPPER - по этой строке скрипт узнаёт себя, не переименовывать.
#
# Обёртка над оригинальным sdb; оригинал лежит рядом как sdb.real. Живёт в тулчейне
# Tizen (<sdktools>/data/tools/sdb), то есть вне репозитория, — эта копия нужна,
# чтобы восстановить её после переустановки SDK, которая затирает тулчейн целиком.
#
#   ./scripts/sdb-wrapper.sh --install                    # путь берётся из SDB в .env
#   ./scripts/sdb-wrapper.sh --install /path/to/tools/sdb # или явно
#
# Идемпотентно: если по адресу уже обёртка, sdb.real не трогается.
#
# Зачем обёртка нужна: см. "Обёртка над sdb" в CLAUDE.md.

if [ "${1:-}" = "--install" ]; then
    set -euo pipefail

    target="${2:-}"
    if [ -z "$target" ]; then
        root="$(cd "$(dirname "$0")/.." && pwd)"
        if [ -f "$root/.env" ]; then
            target="$(. "$root/.env" >/dev/null 2>&1; printf '%s' "${SDB:-}")"
        fi
    fi
    target="${target:-$HOME/.tizen-extension-platform/server/sdktools/data/tools/sdb}"

    [ -e "$target" ] || { echo "ERROR: $target не найден - SDK не установлен?"; exit 1; }
    [ "$0" -ef "$target" ] && { echo "ERROR: $0 и есть установленная обёртка, ставить надо копию из scripts/"; exit 1; }

    # Оригинальный sdb - бинарник, любая обёртка - скрипт с shebang. Отличать надо
    # именно так, а не по маркеру: под маркер не попадёт обёртка прошлой версии,
    # и mv затрёт ею настоящий sdb.real - восстанавливать будет уже нечего.
    if head -c 2 "$target" 2>/dev/null | grep -q '#!'; then
        [ -e "$target.real" ] || { echo "ERROR: $target - скрипт, но $target.real нет; оригинальный sdb потерян, нужна переустановка SDK"; exit 1; }
        echo "по адресу уже обёртка, $target.real не трогаю"
    else
        mv "$target" "$target.real"
        echo "оригинал сохранён: $target.real"
    fi

    cp "$0" "$target"
    chmod +x "$target"
    echo "обёртка установлена: $target"
    exit 0
fi

DIR="$(cd "$(dirname "$0")" && pwd)"
REAL="$DIR/sdb.real"

args=("$@")

# Логируем команды VS Code для диагностики.
# Ошибку записи гасим: лог может однажды создать root (расширение поднимает sdb
# через osascript, если не найдёт запущенный sdb-сервер), и тогда без этого
# посыпались бы все последующие вызовы из-под пользователя.
{
    date
    printf 'sdb'
    printf ' %q' "$@"
    printf '\n'
} >> /tmp/tizen-vscode-sdb.log 2>/dev/null || true

# Compatibility fix for old Samsung TV
for ((i=0; i<${#args[@]}; i++)); do
    if [[ "${args[i]}" == "vd_applist" ]]; then
        args[i]="applist"
    fi
done

# Compatibility fix for old Samsung TV: tizen-core (tz) pushes the package to /
# and installs it from there, but the root filesystem is read-only on 2015 sets —
# the push dies with "failed to close" and vd_appinstall then finds nothing.
# Redirect any root-level .wgt path to the staging directory the platform expects.
STAGE=/opt/usr/apps/tmp
for ((i=0; i<${#args[@]}; i++)); do
    if [[ "${args[i]}" =~ ^/[^/]+\.wgt$ ]]; then
        # bare destination argument, e.g. "push <local> /app.wgt"
        args[i]="$STAGE${args[$i]}"
    elif [[ "${args[i]}" == *" /"*".wgt"* ]]; then
        # path inside one combined argument, e.g. "0 vd_appinstall <id> /app.wgt"
        args[i]="$(printf '%s' "${args[i]}" | sed -E "s#(^| )/([^/ ]+\.wgt)( |\$)#\1${STAGE}/\2\3#g")"
    fi
done

exec "$REAL" "${args[@]}"
