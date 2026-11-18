#!/usr/bin/env bash
set -euo pipefail

src=$(cd "$(dirname "$0")" && pwd)
root=${RTV_ROOT:-$HOME/rhythm-tv}
dst=$root/deploy
label=local.rhythm-tv.homegate
plist=$HOME/Library/LaunchAgents/$label.plist

mkdir -p "$dst" "$root/logs"
if [ ! -f "$dst/.env" ]; then
	echo "нет $dst/.env: скопируй $src/.env.example и заполни" >&2
	exit 1
fi
set -a
. "$dst/.env"
set +a
mkdir -p "$TORRENT_DIR" "$STORE_DIR" "$HLS_DIR"

install -m 644 "$src/compose.yaml" "$src/egress.json" "$src/relay.json" "$src/lanproxy.py" "$dst/"
install -m 755 "$src/homegate.sh" "$src/check-egress.sh" "$dst/"
sing-box check -c "$dst/relay.json"
tmutil addexclusion "$STORE_DIR" "$HLS_DIR" || echo "tmutil addexclusion не удался — исключи из Time Machine руками" >&2

sed -e "s#@DEPLOY@#$dst#g" -e "s#@LOGS@#$root/logs#g" "$src/homegate.plist" >"$plist"
plutil -lint "$plist" >/dev/null
launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$plist"

docker compose --project-directory "$dst" -f "$dst/compose.yaml" up -d
