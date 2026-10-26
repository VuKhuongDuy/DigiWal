#!/usr/bin/env bash
# Sustained run: RATE req/s for MINUTES minutes, in CHUNK_MIN-minute k6 runs so stability over time shows per chunk.
# Needs enough funded users: each accepted withdrawal costs 0.0006 ETH (00-prepare.sh prints how many the users can afford).
#   RATE=500 MINUTES=60 WRITE_PCT=20 ./20-sustained.sh
source "$(cd "$(dirname "$0")" && pwd)/common.sh"
source "$HERE/lib/run.sh"
preflight_clean
RATE=${RATE:-500}; MINUTES=${MINUTES:-60}; CHUNK_MIN=${CHUNK_MIN:-5}; WRITE_PCT=${WRITE_PCT:-20}; MIX=${MIX:-mixed}
OUT=$RESULTS_ROOT/$(date +%Y%m%d-%H%M%S)-sustained-${RATE}rps
mkdir -p "$OUT"; environment_snapshot | tee "$OUT/environment.txt"
chunks=$(( MINUTES / CHUNK_MIN )); : > "$OUT/summary.txt"
echo "step | target | achieved | dropped | failed% | write p50/p95/p99 ms | read p50/p95/p99 ms | PG ledger (vm2) cores/thr%/act | PG gateway (vm3) | PG core (vm3) | backlog" > "$OUT/summary.txt"
for i in $(seq 1 "$chunks"); do
	d=$(printf '%s/chunk-%02d' "$OUT" "$i")
	run_load "$d" "$RATE" $(( CHUNK_MIN * 60 )) 0 "$WRITE_PCT" "$MIX"
	analyse "$d" "$RATE" $(( CHUNK_MIN * 60 )) 0 > /dev/null
	line=$(sed -n '3p' "$d/table.txt" | sed "s/^r$RATE/chunk $i/"); echo "$line" | tee -a "$OUT/summary.txt"
done
echo; cat "$OUT/summary.txt"; echo; [ "${WAIT_IDLE:-1}" = 1 ] && wait_idle 900 | tee -a "$OUT/summary.txt"
echo "results: $OUT"
