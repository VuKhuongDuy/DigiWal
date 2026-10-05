#!/usr/bin/env bash
# Host firewall for a DigiWal node (docs/design/multi-node-deployment.md, "Network design" / invariant 8).
# Idempotent: rebuilds its own chains, leaves everything else alone. Run as root on each node:
#   PEER_PUBLIC_IP=<the other node's public IP> ./firewall.sh [apply|status|off]
#
# Two layers, because Docker-published ports bypass INPUT and ufw (they are DNAT'd and go through FORWARD):
#   DIGIWAL-INPUT  host traffic: loopback, established, the WireGuard network, SSH, and WireGuard's UDP from the peer only
#   DOCKER-USER    traffic to containers: allowed from wg0 and the Docker bridges; a NEW connection that arrives on
#                  the public interface and was DNAT'd to a container is dropped
set -euo pipefail

PEER_PUBLIC_IP=${PEER_PUBLIC_IP:?set PEER_PUBLIC_IP to the public IPv4 of the other node}
WG_IF=${WG_IF:-wg0}
WG_NET=${WG_NET:-10.10.0.0/24}
WG_PORT=${WG_PORT:-51820}
SSH_PORT=${SSH_PORT:-22}
if [ -z "${PUBLIC_IF:-}" ]; then
	PUBLIC_IF=$(ip -4 route show default | awk '{print $5; exit}')
fi
cmd=${1:-apply}

del_jump() { # del_jump <table-chain> <jump rule...>: remove every copy, ignore absence
	local chain=$1; shift
	while iptables -C "$chain" "$@" 2>/dev/null; do iptables -D "$chain" "$@"; done
}

off() {
	del_jump INPUT -j DIGIWAL-INPUT
	iptables -F DIGIWAL-INPUT 2>/dev/null && iptables -X DIGIWAL-INPUT 2>/dev/null || true
	iptables -F DIGIWAL-DOCKER 2>/dev/null || true
	del_jump DOCKER-USER -j DIGIWAL-DOCKER
	iptables -X DIGIWAL-DOCKER 2>/dev/null || true
	del_jump INPUT -j DIGIWAL-INPUT6 2>/dev/null || true
	ip6tables -D INPUT -j DIGIWAL-INPUT6 2>/dev/null || true
	ip6tables -F DIGIWAL-INPUT6 2>/dev/null && ip6tables -X DIGIWAL-INPUT6 2>/dev/null || true
}

apply() {
	off
	# ---- IPv4 host input
	iptables -N DIGIWAL-INPUT
	iptables -A DIGIWAL-INPUT -i lo -j ACCEPT
	iptables -A DIGIWAL-INPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
	iptables -A DIGIWAL-INPUT -p icmp -j ACCEPT
	iptables -A DIGIWAL-INPUT -i "$WG_IF" -s "$WG_NET" -j ACCEPT
	iptables -A DIGIWAL-INPUT -p tcp --dport "$SSH_PORT" -j ACCEPT
	iptables -A DIGIWAL-INPUT -p udp --dport "$WG_PORT" -s "$PEER_PUBLIC_IP" -j ACCEPT
	# Only the public interface is closed: Docker bridge traffic to the host (container -> host services) stays open.
	iptables -A DIGIWAL-INPUT ! -i "$PUBLIC_IF" -j ACCEPT
	iptables -A DIGIWAL-INPUT -j DROP
	iptables -I INPUT 1 -j DIGIWAL-INPUT

	# ---- DOCKER-USER (exists once Docker is running)
	iptables -N DOCKER-USER 2>/dev/null || true
	iptables -N DIGIWAL-DOCKER
	iptables -A DIGIWAL-DOCKER -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
	iptables -A DIGIWAL-DOCKER -i "$WG_IF" -j RETURN
	iptables -A DIGIWAL-DOCKER ! -i "$PUBLIC_IF" -j RETURN
	iptables -A DIGIWAL-DOCKER -i "$PUBLIC_IF" -m conntrack --ctstate DNAT -j DROP
	iptables -A DIGIWAL-DOCKER -j RETURN
	iptables -I DOCKER-USER 1 -j DIGIWAL-DOCKER

	# ---- IPv6: nothing here publishes on IPv6, so allow only SSH, loopback, established, ICMPv6
	ip6tables -N DIGIWAL-INPUT6
	ip6tables -A DIGIWAL-INPUT6 -i lo -j ACCEPT
	ip6tables -A DIGIWAL-INPUT6 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
	ip6tables -A DIGIWAL-INPUT6 -p ipv6-icmp -j ACCEPT
	ip6tables -A DIGIWAL-INPUT6 -p tcp --dport "$SSH_PORT" -j ACCEPT
	ip6tables -A DIGIWAL-INPUT6 ! -i "$PUBLIC_IF" -j ACCEPT
	ip6tables -A DIGIWAL-INPUT6 -j DROP
	ip6tables -I INPUT 1 -j DIGIWAL-INPUT6
}

status() {
	echo "== iptables INPUT (first 3)"; iptables -S INPUT | head -4
	echo "== DIGIWAL-INPUT"; iptables -S DIGIWAL-INPUT 2>&1
	echo "== DOCKER-USER"; iptables -S DOCKER-USER 2>&1
	echo "== DIGIWAL-DOCKER"; iptables -S DIGIWAL-DOCKER 2>&1
}

case $cmd in
	apply) apply; echo "firewall applied (public interface: $PUBLIC_IF, peer: $PEER_PUBLIC_IP)" ;;
	off) off; echo "firewall rules removed" ;;
	status) status ;;
	*) echo "usage: PEER_PUBLIC_IP=... $0 [apply|status|off]" >&2; exit 2 ;;
esac
