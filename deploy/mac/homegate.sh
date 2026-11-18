#!/usr/bin/env bash
set -u

dir=$(cd "$(dirname "$0")" && pwd)
set -a
. "$dir/.env"
set +a

: "${HOME_IP:?HOME_IP обязателен}"
: "${HOME_DHCP_MARK:?HOME_DHCP_MARK обязателен}"
home_if=${HOME_IF:-en0}
lan_port=${LAN_PORT:-8000}
host_port=${HOST_PORT:-8000}
check_sec=${CHECK_SEC:-10}
away_after=${AWAY_AFTER_CHECKS:-3}
singbox=${SINGBOX_BIN:-/opt/homebrew/bin/sing-box}
docker_bin=${DOCKER_BIN:-/usr/local/bin/docker}

mark_hex=$(printf '%s' "$HOME_DHCP_MARK" | xxd -p | tr -d '\n')
relay_pid=
proxy_pid=
state=
away_count=0

log() { printf '%s %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$*"; }

# Серийный номер роутера приходит в DHCP option 125 hex-дампом, разрезанным по строкам, поэтому сверка — в hex.
at_home() {
	[ "$(ipconfig getifaddr "$home_if" 2>/dev/null)" = "$HOME_IP" ] || return 1
	ipconfig getpacket "$home_if" 2>/dev/null |
		awk '/^option_125/{f=1;next} f&&/^[0-9a-f][0-9a-f][0-9a-f][0-9a-f] /{for(i=2;i<=17;i++) if($i ~ /^[0-9a-f][0-9a-f]$/) printf "%s",$i} f&&/^$/{exit}' |
		grep -q "$mark_hex"
}

alive() { [ -n "$1" ] && kill -0 "$1" 2>/dev/null; }

start_relay() {
	"$singbox" run -c "$dir/relay.json" &
	relay_pid=$!
	log "relay started pid=$relay_pid"
}

start_proxy() {
	/usr/bin/python3 "$dir/lanproxy.py" "$HOME_IP" "$lan_port" 127.0.0.1 "$host_port" &
	proxy_pid=$!
	log "lan proxy started pid=$proxy_pid on $HOME_IP:$lan_port"
}

stop_all() {
	local p
	for p in $relay_pid $proxy_pid; do kill "$p" 2>/dev/null; done
	for p in $relay_pid $proxy_pid; do wait "$p" 2>/dev/null; done
	relay_pid=
	proxy_pid=
}

started_at() { "$docker_bin" inspect -f '{{.State.Running}} {{.State.StartedAt}}' "$1" 2>/dev/null; }

# Сервер живёт в сетевом пространстве tms-egress: перезапуск egress оставляет его без сети, лечится только рестартом.
heal_server() {
	local e s
	e=$(started_at tms-egress) || return 0
	s=$(started_at tms-server) || return 0
	[ "${e%% *}" = true ] && [ "${s%% *}" = true ] || return 0
	e=${e#* }
	s=${s#* }
	if [[ "${s%%.*}" < "${e%%.*}" ]]; then
		log "tms-server started before tms-egress ($s < $e), restarting it"
		"$docker_bin" restart tms-server >/dev/null
	fi
}

trap 'stop_all; log "stopped"; exit 0' TERM INT
trap stop_all EXIT

log "homegate up: $home_if=$HOME_IP, lan $lan_port -> 127.0.0.1:$host_port"
while :; do
	if at_home; then
		away_count=0
		alive "$relay_pid" || start_relay
		alive "$proxy_pid" || start_proxy
		[ "$state" = home ] || log "home"
		state=home
	else
		away_count=$((away_count + 1))
		if [ "$state" != away ] && [ "$away_count" -ge "$away_after" ]; then
			stop_all
			log "away: relay and lan proxy stopped"
			state=away
		fi
	fi
	heal_server
	sleep "$check_sec" &
	wait $!
done
