#!/usr/bin/env bash
# A/B test of how the gateways reach the ledger-services, repeated and interleaved, so that one noisy run (about +-20% between identical runs here)
# does not decide. Each run is 50-debug-pg.sh at RATE; the report gives the median and the min-max per variant.
#   lb       the gateways call ledger-lb (HAProxy, mode tcp) on vm3, which spreads connections over both ledger-services
#   direct2  the gateways spread requests over both ledger-services themselves (api.Balancer), no HAProxy in the path
#   direct1  the gateways call one ledger-service directly (8444) -- separates "no HAProxy" from "two instances"
#   poolN    both ledger-services get POSTGRES_MAX_CONNS=N (pool64 = today's setting, pool45, pool10); gateways as direct2
#   dbhotN   like poolN, and the ledger's balance cache is OFF, so every read goes to Postgres (this is where a small pool shows)
#   ROUNDS=3 RATE=2500 STEP_S=90 VARIANTS="pool64 pool45 pool10" ./60-ab.sh
#   DRY_RUN=1 ./60-ab.sh          print the plan only
# Each round runs every variant once, in a rotated order (round 1: lb direct2 direct1; round 2: direct2 direct1 lb; ...), so that slow drift of the
# shared VMs hits all variants alike. After every switch the gateways are recreated and a short warm-up run (not counted) fills their connection
# pools and caches. Takes about 5 minutes per run. Restores the default (the overlay's direct2) at the end, also on Ctrl-C.
source "$(cd "$(dirname "$0")" && pwd)/common.sh"
ROUNDS=${ROUNDS:-3}; RATE=${RATE:-2500}; STEP_S=${STEP_S:-90}; VARIANTS=${VARIANTS:-pool64 pool45 pool10}; WARMUP=${WARMUP:-1}
OUT=$RESULTS_ROOT/$(date +%Y%m%d-%H%M%S)-ab; mkdir -p "$OUT"
EP_BOTH="10.10.0.3:8444,10.10.0.3:8448"
# spec <variant> -> "<gateway endpoints> <ledger pool> <ledger balance cache>"  (an empty endpoint list = through ledger-lb)
spec() {
	case $1 in
		lb)       echo "- 64 1";;
		direct2)  echo "$EP_BOTH 64 1";;
		direct1)  echo "10.10.0.3:8444 64 1";;
		pool[0-9]*)  echo "$EP_BOTH ${1#pool} 1";;
		dbhot[0-9]*) echo "$EP_BOTH ${1#dbhot} 0";;
		*) die "unknown variant $1 (lb, direct2, direct1, poolN, dbhotN)";;
	esac
}

# Both tiers are recreated on every switch, so that every run starts from the same cold state. The overrides live in nodes/loadtest-z-ab.yml (the
# name sorts after loadtest-cache.yml, so it wins over it).
switch() { # variant | restore
	local v=$1 ep="" pool=64 cache=1
	if [ "$v" != restore ]; then read -r ep pool cache <<< "$(spec "$v")"; [ "$ep" = - ] && ep=""; fi
	log "-> ${v}${ep:+ gateways: $ep}${v:+ | ledger pool $pool, balance cache $cache}"
	[ -n "${DRY_RUN:-}" ] && return
	ssh vm4 "MODE=$v POOL=$pool CACHE=$cache bash -s" <<'R'
set -e
cd /root/DigiWal/dev; export MASTER_KEY_HEX=not-on-this-node
rm -f nodes/loadtest-ab.yml
if [ "$MODE" = restore ]; then rm -f nodes/loadtest-z-ab.yml
else
	{ echo "services:"; for s in ledger-service ledger-service-2; do printf '  %s:\n    environment:\n      POSTGRES_MAX_CONNS: "%s"\n      BALANCE_CACHE_ENABLED: "%s"\n' "$s" "$POOL" "$CACHE"; done; } > nodes/loadtest-z-ab.yml
fi
files="-f ../infra/docker-compose.yml -f ../e2e/docker-compose.yml -f docker-compose.yml -f nodes/vm4.yml"
for g in nodes/loadtest-*.yml; do [ -e "$g" ] && files="$files -f $g"; done
docker compose -p digiwal-dev --project-directory ../e2e --env-file .env $files up -d --no-build --no-deps --force-recreate ledger-service ledger-service-2 2>&1 | tail -1
for c in ledger-service-1 ledger-service-2-1; do echo "  $c POSTGRES_MAX_CONNS=$(docker exec digiwal-dev-$c printenv POSTGRES_MAX_CONNS) BALANCE_CACHE_ENABLED=$(docker exec digiwal-dev-$c printenv BALANCE_CACHE_ENABLED)"; done
R
	ssh vm3 "MODE=$v EP='$ep' bash -s" <<'R'
set -e
cd /root/DigiWal/dev; export MASTER_KEY_HEX=not-on-this-node
rm -f nodes/loadtest-ab.yml
if [ "$MODE" = restore ]; then rm -f nodes/loadtest-z-ab.yml
else printf 'services:\n  api-gateway:\n    environment:\n      LEDGER_SERVICE_ENDPOINTS: "%s"\n' "$EP" > nodes/loadtest-z-ab.yml; fi
files="-f ../infra/docker-compose.yml -f ../e2e/docker-compose.yml -f docker-compose.yml -f nodes/vm3.yml"
for g in nodes/loadtest-*.yml; do [ -e "$g" ] && files="$files -f $g"; done
docker compose -p digiwal-dev --project-directory ../e2e --env-file .env $files up -d --no-build --no-deps --force-recreate api-gateway 2>&1 | tail -1
sleep 15
for c in 1 2; do echo "  api-gateway-$c LEDGER_SERVICE_ENDPOINTS='$(docker exec digiwal-dev-api-gateway-$c printenv LEDGER_SERVICE_ENDPOINTS)'"; done
R
}
trap 'echo; log "restoring the default"; switch restore || true' EXIT

read -r -a V <<< "$VARIANTS"; n=${#V[@]}
log "plan: $ROUNDS rounds x ${V[*]} at $RATE req/s for ${STEP_S}s -> $OUT"
: > "$OUT/runs.jsonl"
for ((r = 0; r < ROUNDS; r++)); do
	for ((i = 0; i < n; i++)); do
		v=${V[$(( (i + r) % n ))]}
		log "round $((r + 1))/$ROUNDS, variant $v"
		switch "$v"
		[ -n "${DRY_RUN:-}" ] && continue
		[ "$WARMUP" = 1 ] && { log "warm-up (not counted)"; RATE=1500 STEP_S=30 "$HERE/50-debug-pg.sh" > "$OUT/warmup.log" 2>&1 || log "warm-up failed, see $OUT/warmup.log"; }
		RATE=$RATE STEP_S=$STEP_S "$HERE/50-debug-pg.sh" > "$OUT/run.$r.$v.log" 2>&1 || { log "run failed, see $OUT/run.$r.$v.log"; continue; }
		dir=$(sed -n 's/^results: //p' "$OUT/run.$r.$v.log" | tail -1)
		[ -d "$dir" ] || { log "no result directory in $OUT/run.$r.$v.log"; continue; }
		python3 "$HERE/lib/ab-collect.py" "$dir" | python3 -c "import json,sys; r=json.loads(sys.stdin.read()); r['variant']='$v'; r['round']=$r; print(json.dumps(r))" >> "$OUT/runs.jsonl"
		tail -1 "$OUT/runs.jsonl" | python3 -c "import json,sys; r=json.loads(sys.stdin.read()); print('  ', {k: r.get(k) for k in ('achieved','gateway_avg','k6_p95','failed_pct')})"
	done
done
[ -n "${DRY_RUN:-}" ] && exit 0
echo; python3 "$HERE/lib/ab-report.py" "$OUT/runs.jsonl" | tee "$OUT/report.txt"; echo; echo "results: $OUT"
