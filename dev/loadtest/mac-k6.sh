#!/usr/bin/env bash
# RUN ON THE LAPTOP: opens the tunnels (tunnels.sh) and runs k6 against both gateways. Everything stays inside the repo (k6/keys.json, k6/results/).
#   BASES=http://5.189.153.42:8184,http://164.68.103.182:8184 ./mac-k6.sh   direct to the gateways (no tunnels)
#   ./mac-k6.sh                                  the default step test (balances, 2000..4000 req/s, 60 s per step)
#   STEPS=1000,2000 STEP_S=30 ./mac-k6.sh        other steps; MIX=balances|health|ping|mixed; REFRESH_KEYS=1 copies keys.json again
HERE=$(cd "$(dirname "$0")" && pwd); K6DIR=$HERE/k6
VM2=${VM2:-alex_vm_2}
STEPS=${STEPS:-2000,2500,3000,3500,4000}; STEP_S=${STEP_S:-60}; GAP_S=${GAP_S:-15}; MIX=${MIX:-balances}

command -v k6 >/dev/null || { echo "k6 is not installed (brew install k6)" >&2; exit 1; }
ulimit -n 65536 2>/dev/null || ulimit -n 10240 2>/dev/null || echo "warning: could not raise the open-files limit ($(ulimit -n)); k6 may run out of sockets" >&2

if [ "${REFRESH_KEYS:-0}" = 1 ] || [ ! -s "$K6DIR/keys.json" ]; then
	echo "copying keys.json from $VM2"; scp "$VM2:/tmp/loadtest/keys.json" "$K6DIR/keys.json" || exit 1
fi
if [ -n "${BASES:-}" ]; then :   # direct: BASES=http://<vm3 ip>:8184,http://<vm4 ip>:8184 (needs ./05-laptop-access.sh open <your ip>)
else
	"$HERE/tunnels.sh" up || exit 1   # fallback: SSH tunnels (they reset connections above ~1,500 req/s)
	B=http://127.0.0.1
	BASES=$B:18084,$B:18085,$B:18086,$B:18087,$B:18094,$B:18095,$B:18096,$B:18097
fi
OUT=$K6DIR/results/$(date +%Y%m%d-%H%M%S); mkdir -p "$OUT"
echo "k6: mix=$MIX steps=$STEPS step=${STEP_S}s -> $OUT"
k6 run -e KEYS="$K6DIR/keys.json" -e MIX="$MIX" -e STEPS="$STEPS" -e STEP_S="$STEP_S" -e GAP_S="$GAP_S" -e BASES="$BASES" \
	--summary-export="$OUT/summary.json" "$K6DIR/mix.js" 2>&1 | tee "$OUT/k6.log"
echo "results: $OUT"
