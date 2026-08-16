#!/usr/bin/env bash
# Клиент — строго ES5 (см. CLAUDE.md): WRT на Tizen 2.3 не понимает ни синтаксис
# ES6, ни рантайм-API вроде Promise. Локальный аналог — однострочник на jsc
# из CLAUDE.md, но он только для macOS; здесь то же самое, но переносимо
# и с проверкой API, которых парсер не видит в принципе.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# js/config.js генерируется скриптом сборки и лежит в .gitignore — в CI его нет,
# поэтому список берётся из git, а не из файловой системы.
FILES=()
while IFS= read -r f; do FILES+=("$f"); done < <(git ls-files 'client/js/*.js')
[ "${#FILES[@]}" -gt 0 ] || { echo "ОШИБКА: под контролем версий нет ни одного JS клиента"; exit 1; }

echo "=== синтаксис ES5 (${#FILES[@]} файл(ов)) ==="
npx --yes es-check@7 es5 "${FILES[@]}"

echo "=== рантайм-API, которых нет на прошивке 2015 года ==="
# Парсер их пропускает: это валидный ES5-синтаксис, который падает уже
# на телевизоре с «undefined is not a function», то есть чёрным экраном.
PATTERN='\b(Promise|fetch|Symbol|WeakMap|WeakSet|Proxy|Reflect)\b'
PATTERN+='|\.(includes|find|findIndex|assign|entries|values|startsWith|endsWith|repeat|padStart|padEnd|trimStart|trimEnd)\('
PATTERN+='|\bObject\.(assign|entries|values|keys)\b|\bArray\.from\b'

# Строки-комментарии выбрасываются намеренно: шапка client/js/app.js буквально
# перечисляет эти слова как запрет, и наивный греп падал бы на собственной
# документации файла.
if grep -nE "$PATTERN" "${FILES[@]}" | grep -vE ':[[:space:]]*(\*|//|/\*)'; then
  echo "ОШИБКА: ES5-несовместимый рантайм-API выше (CLAUDE.md: «клиент — строго ES5»)"
  exit 1
fi

echo "OK"
