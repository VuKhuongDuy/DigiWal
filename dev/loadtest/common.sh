#!/usr/bin/env bash
# Shared settings and helpers for the load-test scripts. Run them from vm2 (ssh to vm3 and vm4 must work).
# k6 runs on vm4, next to api-gateway; Postgres is sampled with cgroup counters (no Prometheus needed).
set -euo pipefail

RESULTS_ROOT=${RESULTS_ROOT:-$HOME/loadtest-results}
K6_HOST=${K6_HOST:-vm2}                                  # where k6 runs: vm2, apart from the gateway (vm3) and the ledger service (vm4)
GATEWAY_NODES=${GATEWAY_NODES:-vm3 vm4}                   # one api-gateway per node, each beside a ledger-service
LEDGER_NODES=${LEDGER_NODES:-vm3 vm4}
# k6 spreads its requests over these (no load balancer). Both gateways: each pair (gateway + ledger-service + Redis) is on its own node.
GATEWAY_URLS=${GATEWAY_URLS:-http://10.10.0.2:8084,http://10.10.0.3:8084}
GATEWAY_URL=${GATEWAY_URL:-${GATEWAY_URLS%%,*}}           # the first one: setup and the pre-flight use it
GATEWAY_NODE=${GATEWAY_NODE:-${GATEWAY_NODES%% *}}
REMOTE_DIR=/tmp/loadtest                                 # on the k6 host: keys.json and the k6 scripts
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

log() { printf '%s %s\n' "$(date +%T)" "$*"; }
die() { echo "ERROR: $*" >&2; exit 1; }

# run_on <node> <command...>: vm2 is this machine (no ssh to itself), the others go through ssh.
run_on() { local node=$1; shift; if [ "$node" = vm2 ]; then bash -c "$*"; else ssh "$node" "$*"; fi; }

# On the k6 host (vm2 is this machine, anything else goes through ssh).
on_k6() { run_on "$K6_HOST" "$*"; }
put_k6() { if [ "$K6_HOST" = vm2 ]; then cp "$1" "$2"; else scp -q "$1" "$K6_HOST:$2"; fi; }
get_k6() { if [ "$K6_HOST" = vm2 ]; then cp "$1" "$2"; else scp -q "$K6_HOST:$1" "$2"; fi; }

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

# preflight_clean: a run that starts while the previous one is still being worked off measures the leftovers too
# (the Reaper releasing thousands of locks through the same ledger-service). Refuses unless FORCE=1.
preflight_clean() {
	[ "${FORCE:-0}" = 1 ] && return 0
	local n a b
	n=$(open_withdrawals); a=$(kafka_lag ledger-service); b=$(kafka_lag history-service)
	if [ "${n:-9999}" -gt 50 ] || [ "$a" != 0 ] || [ "$b" != 0 ]; then
		die "the previous run is still being worked off (withdrawals reserved/validated: $n, Kafka lag ledger-service: $a, history-service: $b). Wait, run ./00-prepare.sh, or FORCE=1 to run anyway."
	fi
}
