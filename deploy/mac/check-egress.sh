#!/usr/bin/env bash
set -u

ipify=https://api.ipify.org
home_if=${HOME_IF:-en0}

direct=$(curl -s -m 10 "$ipify" || true)
bound=$(curl -s -m 10 --interface "$home_if" "$ipify" || true)
chain=$(docker exec tms-server wget -qO- -T 15 "$ipify" 2>/dev/null || true)
printf 'direct (маршрут Мака): %s\n' "${direct:-нет}"
printf 'bound to %s:          %s\n' "$home_if" "${bound:-нет}"
printf 'server (через реле):    %s\n' "${chain:-нет}"

relay=$(pgrep -f 'sing-box run -c .*relay.json' | head -1)
if [ -n "$relay" ]; then
	echo "потоки реле pid=$relay по интерфейсам (не loopback):"
	for proto in tcp udp; do
		nettop -n -L 1 -p "$relay" -m "$proto" -J interface 2>/dev/null |
			awk -F, -v p="$proto" 'NR>2 && $2!="" && $2!="lo0" {n[$2]++} END {for (i in n) printf "  %s %s: %d\n", p, i, n[i]}'
	done
else
	echo "реле не запущено"
fi

if [ -n "$chain" ] && [ "$chain" = "$direct" ] && [ "$direct" != "$bound" ]; then
	echo "FAIL: сервер выходит тем же адресом, что маршрут Мака, а не $home_if"
	exit 1
elif [ -n "$chain" ] && [ "$chain" = "$bound" ]; then
	echo "OK: сервер выходит через $home_if"
elif [ -z "$chain" ] && [ -z "$bound" ]; then
	echo "OK: $home_if наружу не ходит (VPN поднят?) — и сервер не ходит никуда"
else
	echo "WARN: выхода у сервера нет, хотя $home_if наружу ходит — смотри homegate.log и tms-egress"
	exit 2
fi
