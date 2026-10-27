#!/usr/bin/env bash
# Turns the api-gateways' rate limits off for a capacity test, or back on afterwards (every node in GATEWAY_NODES). The override lives only on the
# nodes (dev/nodes/loadtest-off.yml, not in the repo), so nothing committed changes and the default limits return with "on".
#   ./02-rate-limits.sh off        RATE_LIMIT_IP_PER_MIN=0 RATE_LIMIT_USER_PER_MIN=0
#   ./02-rate-limits.sh on         remove the override (defaults: 1200 per IP and 600 per key per minute)
source "$(cd "$(dirname "$0")" && pwd)/common.sh"
mode=${1:-}
[ "$mode" = off ] || [ "$mode" = on ] || die "usage: $0 off|on"
for node in $GATEWAY_NODES; do
ssh "$node" "MODE=$mode NODE=$node bash -s" <<'R'
set -e
cd /root/DigiWal/dev
export MASTER_KEY_HEX=not-on-this-node
f=nodes/loadtest-off.yml
files="-f ../infra/docker-compose.yml -f ../e2e/docker-compose.yml -f docker-compose.yml -f nodes/$NODE.yml"
if [ "$MODE" = off ]; then
	printf 'services:\n  api-gateway:\n    environment:\n      RATE_LIMIT_IP_PER_MIN: "0"\n      RATE_LIMIT_USER_PER_MIN: "0"\n' > $f
else
	rm -f $f
fi
for g in nodes/loadtest-*.yml; do [ -e "$g" ] && files="$files -f $g"; done
docker compose -p digiwal-dev --project-directory ../e2e --env-file .env $files up -d --no-build --no-deps --force-recreate api-gateway 2>&1 | tail -1
sleep 5
echo "$NODE api-gateway: ip=$(docker exec digiwal-dev-api-gateway-1 printenv RATE_LIMIT_IP_PER_MIN || echo default) user=$(docker exec digiwal-dev-api-gateway-1 printenv RATE_LIMIT_USER_PER_MIN || echo default)"
R
done
