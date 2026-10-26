#!/usr/bin/env bash
# Shared settings and helpers for the load-test scripts. Run them from vm2 (ssh to vm3 and vm4 must work).
# k6 runs on vm4, next to api-gateway; Postgres is sampled with cgroup counters (no Prometheus needed).
set -euo pipefail

RESULTS_ROOT=${RESULTS_ROOT:-$HOME/loadtest-results}
K6_HOST=${K6_HOST:-vm4}                                  # where k6 runs
GATEWAY_URL=${GATEWAY_URL:-http://127.0.0.1:8084}        # api-gateway as seen from the k6 host
REMOTE_DIR=/tmp/loadtest                                 # on the k6 host: keys.json and the k6 scripts
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

log() { printf '%s %s\n' "$(date +%T)" "$*"; }
die() { echo "ERROR: $*" >&2; exit 1; }

# run_on <node> <command...>: vm2 is this machine (no ssh to itself), the others go through ssh.
run_on() { local node=$1; shift; if [ "$node" = vm2 ]; then bash -c "$*"; else ssh "$node" "$*"; fi; }

# psql inside the Postgres container of a node. vm2: ledger. vm3: custody, withdrawal, chain_watcher, auditor.
# vm4 has postgres-gateway (api_gateway) instead. Usage: sql vm3 withdrawal "select 1"
sql() {
	local node=$1 db=$2 q=$3 c=digiwal-dev-postgres-1
	[ "$node" = vm4 ] && c=digiwal-dev-postgres-gateway-1
	if [ "$node" = vm2 ]; then docker exec "$c" psql -U custody -d "$db" -tA -c "$q"
	else ssh "$node" "docker exec $c psql -U custody -d $db -tA -c \"$q\""; fi
}

# Consumer-group lag on deposits.confirmed (the topic the ledger credits from). Prints a number, or "?" when the group has no member.
kafka_lag() {
	ssh vm3 "docker exec digiwal-dev-kafka-1 /opt/kafka/bin/kafka-consumer-groups.sh --bootstrap-server localhost:9092 --describe --group $1 2>/dev/null | awk '/deposits.confirmed/{print \$6}'" | head -1 | sed 's/^$/?/'
}

# Withdrawals accepted but not yet picked up by the signing pipeline (reserved) or not yet locked (validated). While the chain is
# down they stay here until the Reaper fails them after 5 minutes; with the chain up this is the pipeline's backlog.
open_withdrawals() { sql vm3 withdrawal "select count(*) from withdrawal_requests where status in ('reserved','validated')"; }
