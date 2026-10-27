#!/usr/bin/env bash
# Pre-flight for a load test. Changes nothing except: it (re)writes keys.json on the k6 host from the simulator's state,
# and gives every simulated user an active whitelisted address. Exit code 1 if the system is not ready.
#   WAIT_MIN=45 ./00-prepare.sh     minutes to wait for the Kafka consumers and the pipeline backlog to drain
source "$(cd "$(dirname "$0")" && pwd)/common.sh"
WAIT_MIN=${WAIT_MIN:-45}
fail=0
ok()  { printf '  ok    %s\n' "$*"; }
bad() { printf '  FAIL  %s\n' "$*"; fail=1; }

echo "1. services"
check() { # node container...
	local node=$1; shift
	for c in "$@"; do
		st=$(run_on "$node" "docker inspect -f '{{.State.Status}} {{.State.Restarting}} {{if .State.Health}}{{.State.Health.Status}}{{else}}-{{end}}' digiwal-dev-$c-1 2>/dev/null" || true)
		case "$st" in "running false healthy"|"running false -") ;; *) bad "$node $c: ${st:-missing}";; esac
	done
}
check vm2 mongo
check vm3 postgres postgres-gateway kafka redis anvil wallet-service chain-watcher sweeper api-gateway ledger-lb
check vm4 postgres redis ledger-service withdrawal-service history-service
[ $fail = 0 ] && ok "all functional services up"
sim=$(ssh vm3 "docker inspect -f '{{.State.Status}}' digiwal-dev-simulator-1 2>/dev/null" || echo missing)
[ "$sim" = running ] && bad "the simulator is running and would add background load (docker stop digiwal-dev-simulator-1 on vm3)" || ok "simulator is stopped"

echo "1b. api-gateway rate limits (a load test from one IP is rejected with 429 beyond the IP limit)"
on=""
for n in $GATEWAY_NODES; do
	ip=$(ssh "$n" "docker exec digiwal-dev-api-gateway-1 printenv RATE_LIMIT_IP_PER_MIN" 2>/dev/null || true)
	us=$(ssh "$n" "docker exec digiwal-dev-api-gateway-1 printenv RATE_LIMIT_USER_PER_MIN" 2>/dev/null || true)
	{ [ "${ip:-1200}" = 0 ] && [ "${us:-600}" = 0 ]; } || on="$on $n(ip=${ip:-1200}/min user=${us:-600}/min)"
done
if [ -z "$on" ]; then ok "rate limits disabled on every gateway ($GATEWAY_NODES)"
else bad "rate limits are on:$on: most k6 requests would get 429. ./02-rate-limits.sh off"; fi

echo "2. Kafka consumers have caught up (deposits.confirmed lag; the ledger credits from it)"
end=$(( $(date +%s) + WAIT_MIN * 60 ))
while :; do
	a=$(kafka_lag ledger-service); b=$(kafka_lag history-service)
	if [ "$a" = 0 ] && [ "$b" = 0 ]; then ok "lag 0 for ledger-service and history-service"; break; fi
	[ "$a" = "?" ] || [ "$b" = "?" ] && { bad "a consumer group has no members (ledger-service lag=$a, history-service lag=$b): restart that service on vm4"; break; }
	[ "$(date +%s)" -ge "$end" ] && { bad "lag still ledger-service=$a history-service=$b after ${WAIT_MIN} min"; break; }
	log "waiting: ledger-service lag=$a history-service lag=$b"; sleep 30
done

echo "3. the withdrawal pipeline is idle"
while :; do
	n=$(open_withdrawals)
	[ "${n:-9999}" -lt 20 ] && { ok "$n withdrawals reserved/validated"; break; }
	[ "$(date +%s)" -ge "$end" ] && { bad "$n withdrawals still reserved/validated"; break; }
	log "waiting: $n withdrawals reserved/validated"; sleep 30
done

echo "4. API keys and whitelists for the simulated users"
state=$(ssh vm3 "cat /var/lib/docker/volumes/digiwal-dev_sim_data/_data/state.json")
echo "$state" | python3 -c "import json,sys; k=[u['api_key'] for u in json.load(sys.stdin)['users'] if u.get('api_key')]; print(json.dumps(k))" \
	| on_k6 "mkdir -p $REMOTE_DIR && umask 077 && cat > $REMOTE_DIR/keys.json"
if on_k6 "BASE=$GATEWAY_URL python3 -" <<'PY'

import json, os, random, time, urllib.request
base = os.environ["BASE"]; keys = json.load(open("/tmp/loadtest/keys.json"))
def call(k, method, path, body=None):
    r = urllib.request.Request(base + path, method=method, data=json.dumps(body).encode() if body else None,
                               headers={"Authorization": "Bearer " + k, "Content-Type": "application/json"})
    with urllib.request.urlopen(r, timeout=20) as x:
        return json.load(x)
def active(k): return [w for w in call(k, "GET", "/v1/whitelist") if w["status"] == "active"]
missing = [k for k in keys if not active(k)]
for k in missing:
    call(k, "POST", "/v1/whitelist", {"address": "0x" + "".join(random.choice("0123456789abcdef") for _ in range(40))})
for _ in range(30):                       # the dev cooldown is a couple of seconds
    missing = [k for k in missing if not active(k)]
    if not missing: break
    time.sleep(2)
eth = "11111111-1111-1111-1111-111111111111"
tot = n_funded = 0
for k in keys:
    for b in call(k, "GET", "/v1/balances"):
        if b["asset_id"] == eth:
            a = int(b["available_balance"]); tot += a; n_funded += a >= 600000000000000
print(f"  keys: {len(keys)}, still without an active whitelist: {len(missing)}")
print(f"  users able to withdraw (>= 0.0006 ETH): {n_funded}; total available {tot/1e18:.3f} ETH = {tot//600000000000000} minimum-size withdrawals")
raise SystemExit(1 if missing or n_funded == 0 else 0)
PY
then ok "keys and whitelists ready"; else bad "keys/whitelists/balances not ready (users without ETH: run 01-fund-users.sh)"; fi

[ $fail = 0 ] && echo "READY" || { echo "NOT READY"; exit 1; }
