#!/usr/bin/env bash
# Switches the read caches on or off (docs/design/read-cache.md): the API-key cache in api-gateway (vm3) and the balance cache in
# ledger-service (vm4). The override files live on the nodes only (dev/nodes/loadtest-cache.yml, not in the repo).
#   ./03-cache.sh on          AUTH_CACHE_ENABLED=1, BALANCE_CACHE_ENABLED=1
#   ./03-cache.sh off         remove the overrides (the flags default to off)
#   ./03-cache.sh status      what each service has now, and the cache counters
source "$(cd "$(dirname "$0")" && pwd)/common.sh"
mode=${1:-status}
recreate() { # node service yaml
	ssh "$1" "NODE=$1 SVC=$2 YAML='$3' MODE=$mode bash -s" <<'R'
set -e
cd /root/DigiWal/dev; export MASTER_KEY_HEX=not-on-this-node
if [ "$MODE" = on ]; then printf '%b' "$YAML" > nodes/loadtest-cache.yml; else rm -f nodes/loadtest-cache.yml; fi
files="-f ../infra/docker-compose.yml -f ../e2e/docker-compose.yml -f docker-compose.yml -f nodes/$NODE.yml"
for f in nodes/loadtest-*.yml; do [ -e "$f" ] && files="$files -f $f"; done
docker compose -p digiwal-dev --project-directory ../e2e --env-file .env $files up -d --no-build --no-deps --force-recreate "$SVC" 2>&1 | tail -1
R
}
case $mode in
	on|off)
		recreate "$GATEWAY_NODE" api-gateway 'services:\n  api-gateway:\n    environment:\n      AUTH_CACHE_ENABLED: "1"\n'
		recreate vm4 ledger-service 'services:\n  ledger-service:\n    environment:\n      BALANCE_CACHE_ENABLED: "1"\n'
		sleep 5 ;;
	status) ;;
	*) die "usage: $0 on|off|status" ;;
esac
echo "api-gateway    AUTH_CACHE_ENABLED=$(ssh "$GATEWAY_NODE" 'docker exec digiwal-dev-api-gateway-1 printenv AUTH_CACHE_ENABLED' 2>/dev/null || echo unset)"
echo "ledger-service BALANCE_CACHE_ENABLED=$(ssh vm4 'docker exec digiwal-dev-ledger-service-1 printenv BALANCE_CACHE_ENABLED' 2>/dev/null || echo unset)"
echo "--- cache counters (since the services started)"
ssh "$GATEWAY_NODE" "docker exec digiwal-dev-api-gateway-1 wget -qO- http://127.0.0.1:9100/metrics" 2>/dev/null | grep -E '^digiwal_cache_(requests|invalidations)_total' | sed 's/^/  /'
ssh vm4 "docker exec digiwal-dev-ledger-service-1 wget -qO- http://127.0.0.1:9100/metrics" 2>/dev/null | grep -E '^digiwal_cache_(requests|invalidations)_total' | sed 's/^/  /'
