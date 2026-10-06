#!/usr/bin/env bash
# Checks this node of the two-node dev environment (docs/design/multi-node-deployment.md, "make verify").
# Run on a node: NODE=vm2|vm3|vm4 ./verify.sh (make verify sets it). Exit code 1 on any failure.
set -uo pipefail
NODE=${NODE:?NODE is empty: this check only makes sense on a node of the two-node layout}
fail=0
ok()  { printf '  ok    %s\n' "$*"; }
bad() { printf '  FAIL  %s\n' "$*"; fail=1; }

PEER_PUBLIC_IP=$(. /etc/digiwal/firewall.env && echo "${PEER_PUBLIC_IP%%,*}") # first peer; several on a three-node mesh
case $NODE in
	vm2) PEER_PRIVATE=10.10.0.2; PEER_OPEN="9092 8545 8443 8080 5432 9100 8081 9187" ;;
	vm3) PEER_PRIVATE=10.10.0.1; PEER_OPEN="5432 27017 9100 8081 9187" ;;
	vm4) PEER_PRIVATE=10.10.0.1; PEER_OPEN="5432 27017 9100 8081 9187" ;;
	*) echo "unknown node $NODE"; exit 2 ;;
esac
tcp() { timeout 3 bash -c "echo > /dev/tcp/$1/$2" 2>/dev/null; }

echo "1. WireGuard"
hs=$(wg show wg0 latest-handshakes | awk '{print $2}' | head -1)
if [ -n "$hs" ] && [ $(( $(date +%s) - hs )) -lt 180 ]; then ok "last handshake $(( $(date +%s) - hs ))s ago"; else bad "no recent handshake"; fi
if ping -c 2 -W 2 -M do -s 1372 "$PEER_PRIVATE" >/dev/null 2>&1; then ok "don't-fragment ping at MTU 1400 to $PEER_PRIVATE"; else bad "DF ping to $PEER_PRIVATE failed (MTU)"; fi

echo "2. The peer's public address exposes nothing but SSH (checked from here)"
for p in 5432 6379 9092 27017 8545 8084 8443 8444 8446 8080 8085 9090 3000 9100 9101 9102 9187 8081 9308; do
	if tcp "$PEER_PUBLIC_IP" "$p"; then bad "port $p is open on the peer's public IP"; fi
done
tcp "$PEER_PUBLIC_IP" 22 && ok "ssh reachable, other service ports closed" || bad "ssh closed on the peer"

echo "3. The peer's private address serves what it should"
for p in $PEER_OPEN; do tcp "$PEER_PRIVATE" "$p" && ok "$PEER_PRIVATE:$p reachable" || bad "$PEER_PRIVATE:$p not reachable"; done

echo "4. Containers on this node"
cd "$(dirname "$0")"
unhealthy=$(docker ps --filter name=digiwal-dev --format '{{.Names}} {{.Status}}' | grep -Ei 'restarting|unhealthy|exited' || true)
n=$(docker ps --filter name=digiwal-dev -q | wc -l)
[ -z "$unhealthy" ] && ok "$n containers up, none restarting or unhealthy" || bad "$unhealthy"
restarts=$(docker ps --filter name=digiwal-dev -q | xargs -r docker inspect -f '{{.Name}} {{.RestartCount}}' | awk '$2>0')
[ -z "$restarts" ] && ok "no container has restarted" || echo "  note  restarts: $(echo $restarts)"

echo "5. Published ports are not on a public address"
exposed=$(docker ps --filter name=digiwal-dev --format '{{.Names}} {{.Ports}}' | grep -E '(^| )0\.0\.0\.0:|\[::\]:' || true)
[ -z "$exposed" ] && ok "no container publishes on 0.0.0.0 or [::]" || bad "published on all interfaces: $exposed"

if [ "$NODE" = vm3 ]; then
	echo "6. Monitoring (Prometheus on this node)"
	down=$(curl -sf http://127.0.0.1:9090/api/v1/targets | python3 -c '
import json, sys
t = json.load(sys.stdin)["data"]["activeTargets"]
print(len(t), *[x["scrapeUrl"] for x in t if x["health"] != "up"])') || down=""
	set -- $down
	if [ -z "$down" ]; then bad "Prometheus does not answer on 127.0.0.1:9090"
	elif [ $# = 1 ]; then ok "all $1 scrape targets are up"
	else n=$1; shift; bad "$# of $n scrape targets down: $*"; fi
	stale=$(curl -sf 'http://127.0.0.1:9090/api/v1/query' --data-urlencode 'query=time() - digiwal_job_last_success_timestamp_seconds > 900' | python3 -c '
import json, sys
print(*[x["metric"]["job"] for x in json.load(sys.stdin)["data"]["result"]])') || stale="?"
	[ -z "$stale" ] && ok "every auditor job succeeded in the last 15 minutes" || bad "auditor jobs without a recent success: $stale"
fi

[ "$fail" = 0 ] && echo "RESULT: all checks passed" || echo "RESULT: FAILED"
exit $fail
