#!/usr/bin/env bash
# A short load plus a bottleneck report: CPU of every container on every node, host user/system time, time per request at
# the gateway and at the ledger, DB pool waits, and what the Postgres containers did. Needs no Prometheus.
#   RATE=800 SECONDS_RUN=40 MIX=balances ./30-diagnose.sh
source "$(cd "$(dirname "$0")" && pwd)/common.sh"
source "$HERE/lib/run.sh"
preflight_clean
RATE=${RATE:-800}; SECONDS_RUN=${SECONDS_RUN:-40}; MIX=${MIX:-balances}; WRITE_PCT=${WRITE_PCT:-20}
OUT=$RESULTS_ROOT/$(date +%Y%m%d-%H%M%S)-diagnose-${RATE}rps; mkdir -p "$OUT/m1" "$OUT/m2"
metrics() { for s in api-gateway ledger-service; do ssh vm4 "docker exec digiwal-dev-$s-1 wget -qO- http://127.0.0.1:9100/metrics" > "$1/$s.txt" 2>/dev/null || true; done; }
metrics "$OUT/m1"
total=$(( SECONDS_RUN + 30 ))
bash "$HERE/lib/nodecpu.sh" "$total" > "$OUT/nodecpu.vm2.txt" 2>/dev/null &
ssh vm3 "bash -s $total" < "$HERE/lib/nodecpu.sh" > "$OUT/nodecpu.vm3.txt" 2>/dev/null &
ssh vm4 "bash -s $total" < "$HERE/lib/nodecpu.sh" > "$OUT/nodecpu.vm4.txt" 2>/dev/null &
run_load "$OUT" "$RATE" "$SECONDS_RUN" 0 "$WRITE_PCT" "$MIX"
metrics "$OUT/m2"; sleep 6
echo; analyse "$OUT" "$RATE" "$SECONDS_RUN" 0; echo
python3 "$HERE/lib/diagnose.py" "$OUT" "$SECONDS_RUN" "$OUT/m1" "$OUT/m2" | tee "$OUT/diagnosis.txt"
echo "results: $OUT"
