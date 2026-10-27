#!/usr/bin/env bash
# Server-side view while YOU run k6 somewhere else (e.g. on a laptop): one line every 15 s with the CPU of the three nodes, the containers
# that matter, DB pool waits, cache hit ratios and the gateways' request rate. Saved to ~/loadtest-results/<time>-watch.txt for later.
#   ./40-watch.sh 1200            watch for 20 minutes
source "$(cd "$(dirname "$0")" && pwd)/common.sh"
mkdir -p "$RESULTS_ROOT"; out="$RESULTS_ROOT/$(date +%Y%m%d-%H%M%S)-watch.txt"
echo "writing $out"; python3 "$HERE/lib/watch.py" "${1:-1200}" "${2:-15}" | tee "$out"
