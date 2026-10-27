#!/usr/bin/env bash
# RUN ON THE LAPTOP. The api-gateways listen only on the VMs' own localhost (never on a public address), so the laptop reaches them through SSH
# tunnels: 4 per gateway (one TCP connection per tunnel, so a lost packet stalls only that tunnel's share of the connections).
#   ./tunnels.sh up | down | status
# ssh aliases come from ~/.ssh/config (override with VM3=... VM4=...). Needs bash 3.2+ (macOS) and lsof.
VM3=${VM3:-alex_vm_3}; VM4=${VM4:-alex_vm_4}
PORTS3=${PORTS3:-"18084 18085 18086 18087"}; PORTS4=${PORTS4:-"18094 18095 18096 18097"}

open_tunnel() { # host localport
	if lsof -nP -iTCP:"$2" -sTCP:LISTEN >/dev/null 2>&1; then echo "port $2 is already listening"; return; fi
	if ssh -f -N -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 -o ServerAliveCountMax=3 -L "$2:127.0.0.1:8084" "$1"; then echo "opened  $2 -> $1:8084"
	else echo "FAILED to open $2 -> $1 (is the ssh alias right? try: ssh $1 true)" >&2; return 1; fi
}

case "${1:-status}" in
	up)
		ulimit -n 65536 2>/dev/null || ulimit -n 10240 2>/dev/null || true
		fail=0
		for p in $PORTS3; do open_tunnel "$VM3" "$p" || fail=1; done
		for p in $PORTS4; do open_tunnel "$VM4" "$p" || fail=1; done
		[ $fail = 0 ] || exit 1
		exec "$0" status ;;
	down)
		for p in $PORTS3 $PORTS4; do
			pids=$(lsof -t -nP -iTCP:"$p" -sTCP:LISTEN 2>/dev/null)
			if [ -n "$pids" ]; then kill $pids && echo "closed  $p"; fi
		done ;;
	status)
		bad=0
		for p in $PORTS3 $PORTS4; do
			code=$(curl -s -o /dev/null -m 5 -w '%{http_code}' "http://127.0.0.1:$p/healthz" 2>/dev/null)
			echo "port $p -> /healthz $code"; [ "$code" = 200 ] || bad=1
		done
		[ $bad = 0 ] && echo "all tunnels reach a gateway" || { echo "some tunnels do not work: ./tunnels.sh down; ./tunnels.sh up" >&2; exit 1; } ;;
	*) echo "usage: $0 up|down|status" >&2; exit 2 ;;
esac
