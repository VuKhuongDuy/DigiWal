#!/usr/bin/env bash
# Step test: rising arrival rates, to find where latency or errors start to climb. Prints one row per step.
#   WAIT_IDLE=0 skips the wait for the pipeline to drain after the table.
#   MIX=balances STEPS=500,1000,1500,2000 ./10-step-test.sh   only GET /v1/balances
#   STEPS=100,200,300,400,500 STEP_S=60 WRITE_PCT=20 ./10-step-test.sh
source "$(cd "$(dirname "$0")" && pwd)/common.sh"
source "$HERE/lib/run.sh"
STEPS=${STEPS:-100,200,300,400,500}; STEP_S=${STEP_S:-60}; GAP_S=${GAP_S:-15}; WRITE_PCT=${WRITE_PCT:-20}; MIX=${MIX:-mixed}
OUT=$RESULTS_ROOT/$(date +%Y%m%d-%H%M%S)-steps
mkdir -p "$OUT"; environment_snapshot | tee "$OUT/environment.txt"
echo "mix: $MIX" | tee -a "$OUT/environment.txt"
run_load "$OUT" "$STEPS" "$STEP_S" "$GAP_S" "$WRITE_PCT" "$MIX"
echo; analyse "$OUT" "$STEPS" "$STEP_S" "$GAP_S"
echo; [ "${WAIT_IDLE:-1}" = 1 ] && wait_idle 600 | tee -a "$OUT/table.txt"
echo "results: $OUT"
