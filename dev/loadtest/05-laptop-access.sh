#!/usr/bin/env bash
# TEMPORARY: lets ONE laptop IP reach the two api-gateways directly on port 8184 (plain HTTP), so k6 can run on the laptop without SSH tunnels
# (which reset connections above ~1,500 req/s). The gateways stay closed to everyone else: the host firewall drops DNAT'd traffic from the public
# interface (infra/node/firewall.sh), and this adds one RETURN rule for the laptop IP and the original destination port 8184 ahead of that drop.
# `make verify` fails while this is open (a published port on a public address). Close it as soon as the test is over.
#   ./05-laptop-access.sh open 203.0.113.7     publish :8184 on the public interface and allow that IP
#   ./05-laptop-access.sh close                remove the published port and the firewall rule
#   ./05-laptop-access.sh status
source "$(cd "$(dirname "$0")" && pwd)/common.sh"
mode=${1:-status}; ip=${2:-}
case $mode in open) [[ $ip =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "usage: $0 open <ipv4 of the laptop>";; close|status) ;; *) die "usage: $0 open <ip>|close|status";; esac
for node in $GATEWAY_NODES; do
	echo "== $node"
	ssh "$node" "MODE=$mode NODE=$node IP=$ip bash -s" <<'R'
set -e
PUBIF=$(ip -4 route show default | awk '{print $5; exit}')
rules() { iptables -S DIGIWAL-DOCKER 2>/dev/null | grep -- '--ctorigdstport 8184' || true; }
case $MODE in
open|close)
	cd /root/DigiWal/dev; export MASTER_KEY_HEX=not-on-this-node
	f=nodes/loadtest-public.yml
	if [ "$MODE" = open ]; then printf 'services:\n  api-gateway:\n    ports: ["0.0.0.0:8184:8080"]\n' > $f; else rm -f $f; fi
	files="-f ../infra/docker-compose.yml -f ../e2e/docker-compose.yml -f docker-compose.yml -f nodes/$NODE.yml"
	for g in nodes/loadtest-*.yml; do [ -e "$g" ] && files="$files -f $g"; done
	docker compose -p digiwal-dev --project-directory ../e2e --env-file .env $files up -d --no-build --no-deps --force-recreate api-gateway 2>&1 | tail -1
	rules | sed 's/^-A/-D/' | while read -r r; do iptables $r; done                      # no duplicates, and close removes it
	if [ "$MODE" = open ]; then
		n=$(iptables -L DIGIWAL-DOCKER -n --line-numbers | awk '/DROP/{print $1; exit}')  # the rule number of the DROP: insert just before it
		iptables -I DIGIWAL-DOCKER "$n" -i "$PUBIF" -s "$IP" -p tcp -m conntrack --ctorigdstport 8184 -j RETURN
	fi ;;
esac
echo "published: $(docker port digiwal-dev-api-gateway-1 8080/tcp | tr '\n' ' ')"
echo "firewall (DIGIWAL-DOCKER):"; iptables -L DIGIWAL-DOCKER -n -v --line-numbers | sed 's/^/   /'
R
done
