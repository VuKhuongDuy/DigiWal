#!/usr/bin/env bash
# Debug run for ONE arrival rate: the same k6 load as 10-step-test.sh, plus a look inside the ledger Postgres and the services' pools while it runs.
#   RATE=4000 STEP_S=90 ./50-debug-pg.sh
# What it leaves in ~/loadtest-results/<time>-pgdebug/:
#   summary.txt     connections over time (active / idle / idle in transaction / waiting, who holds them, what they wait for), pool gauges,
#                   and how long each hop takes (gateway request, ledger request, wait for a connection)
#   statements.txt  pg_stat_statements for the run (reset just before): calls, mean/max/stddev exec time per statement, top by total and by max;
#                   pg_stat_database counters before and after
#   nodeprof.vm3/vm4.txt  CPU split (user/system/softirq/steal), softirqs, CPU pressure and pidstat of every process, per node (also in summary.txt)
#   samples.vm4.txt the raw samples, metrics.*.txt the services' /metrics before and after, k6.log, table.txt the usual k6 table
# It creates the pg_stat_statements extension in the ledger database if it is not there (the library is preloaded; CREATE EXTENSION adds only a view).
source "$(cd "$(dirname "$0")" && pwd)/common.sh"
source "$HERE/lib/run.sh"
RATE=${RATE:-4000}; STEP_S=${STEP_S:-90}; GAP_S=15; MIX=balances; WRITE_PCT=0
preflight_clean
OUT=$RESULTS_ROOT/$(date +%Y%m%d-%H%M%S)-pgdebug; mkdir -p "$OUT"
PSQL="docker exec -i digiwal-dev-postgres-1 psql -U custody -d ledger -X -q -P pager=off"

metrics() { # phase
	for c in api-gateway-1 api-gateway-2; do ssh vm3 "docker exec digiwal-dev-$c wget -qO- http://127.0.0.1:9100/metrics" > "$OUT/metrics.$1.api-gateway.$c.txt" 2>/dev/null || true; done
	for c in ledger-service-1 ledger-service-2-1; do ssh vm4 "docker exec digiwal-dev-$c wget -qO- http://127.0.0.1:9100/metrics" > "$OUT/metrics.$1.ledger-service.$c.txt" 2>/dev/null || true; done
}
dbstats() { echo "-- pg_stat_database ($1)"; ssh vm4 "$PSQL -c \"select numbackends, xact_commit, xact_rollback, blks_read, blks_hit, tup_returned, tup_fetched, tup_inserted, tup_updated, conflicts, temp_files, deadlocks from pg_stat_database where datname='ledger'\""; } 

environment_snapshot | tee "$OUT/environment.txt"
ssh vm4 "$PSQL -c 'create extension if not exists pg_stat_statements' -c 'select pg_stat_statements_reset()'" > /dev/null || die "could not prepare pg_stat_statements on vm4"
{ dbstats before; } > "$OUT/statements.txt"
metrics before

total=$((STEP_S + 100))      # the sampler starts first; k6's setup takes a while before the load starts (k6.log: SETUP_DONE)
ssh vm4 "bash -s $total 2" < "$HERE/lib/pgdebug-sampler.sh" > "$OUT/samples.vm4.txt" 2>/dev/null &
sp=$!
for n in vm3 vm4; do ssh "$n" "bash -s $total 5" < "$HERE/lib/node-prof.sh" > "$OUT/nodeprof.$n.txt" 2>/dev/null & eval "np_$n=\$!"; done
run_load "$OUT" "$RATE" "$STEP_S" "$GAP_S" "$WRITE_PCT" "$MIX"
metrics after
{ dbstats after
  echo; echo "-- pg_stat_statements, by total time (this run only)"
  ssh vm4 "$PSQL -c \"select calls, round(total_exec_time::numeric) total_ms, round(mean_exec_time::numeric,3) mean_ms, round(max_exec_time::numeric,1) max_ms, round(stddev_exec_time::numeric,2) sd_ms, rows, left(regexp_replace(query,'\\s+',' ','g'),100) query from pg_stat_statements where dbid=(select oid from pg_database where datname='ledger') order by total_exec_time desc limit 12\""
  echo; echo "-- by slowest single execution"
  ssh vm4 "$PSQL -c \"select calls, round(mean_exec_time::numeric,3) mean_ms, round(max_exec_time::numeric,1) max_ms, left(regexp_replace(query,'\\s+',' ','g'),100) query from pg_stat_statements where dbid=(select oid from pg_database where datname='ledger') order by max_exec_time desc limit 6\""
  echo; echo "-- all statements together"
  ssh vm4 "$PSQL -c \"select sum(calls) calls, round((sum(total_exec_time)/nullif(sum(calls),0))::numeric,3) mean_ms from pg_stat_statements where dbid=(select oid from pg_database where datname='ledger')\""
} >> "$OUT/statements.txt"
kill "$sp" $np_vm3 $np_vm4 2>/dev/null; wait "$sp" $np_vm3 $np_vm4 2>/dev/null || true
analyse "$OUT" "$RATE" "$STEP_S" "$GAP_S" > /dev/null
{ python3 "$HERE/lib/pgdebug.py" "$OUT" "$STEP_S"; echo; echo "== where the CPU goes on each node (lib/node-prof.sh: /proc/stat, softirqs, pressure, pidstat)"; python3 "$HERE/lib/nodeprof.py" "$OUT" "$STEP_S"; } | tee "$OUT/summary.txt"
echo; cat "$OUT/table.txt"; echo; echo "pg_stat_statements: $OUT/statements.txt"; echo "results: $OUT"
