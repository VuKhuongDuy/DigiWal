#!/usr/bin/env bash
# Checks this node of the multi-node dev environment (vm2, vm3, vm4) (docs/design/multi-node-deployment.md, key-custody-hsm.md 5.6 "make verify").
# Run on a node: NODE=vm2|vm3|vm4 ./verify.sh (make verify sets it). Exit code 1 on any FAIL.
# Every check prints PASS or FAIL with the reason; SKIP means it cannot be decided on this host (it needs the node itself: Linux, root, /proc) and says
# what to do. A SKIP never hides a FAIL elsewhere, but it is not a pass either: run `make -C dev verify` ON the node for those.
# NODE=local checks a single-host stack instead (the e2e stack): PROJECT=digiwal-e2e HSM_DIR=../e2e/hsm-config SWEEP_DIR=../e2e/sweep-config
# SIGNER_BACKEND=pkcs11 NODE=local ./verify.sh -- the WireGuard/peer sections do not apply there and are skipped.
set -uo pipefail
NODE=${NODE:?NODE is empty: this check only makes sense on a node of the multi-node layout (or NODE=local for a single host)}
PROJECT=${PROJECT:-digiwal-dev}
fail=0; skipped=0
ok()   { printf '  PASS  %s\n' "$*"; }
bad()  { printf '  FAIL  %s\n' "$*"; fail=1; }
skip() { printf '  SKIP  %s\n' "$*"; skipped=$((skipped+1)); }
note() { printf '  note  %s\n' "$*"; }
# the node itself: Linux with /proc and root (ownership, kernel settings and other processes' limits are only meaningful there)
on_node() { [ "$(uname -s)" = Linux ] && [ "$(id -u)" = 0 ]; }
NEEDS_NODE="needs the node itself (Linux, root): run 'make -C dev verify' on vm2/vm3"
# mode and owner uid of a path, GNU or BSD stat: "400 10001"
mo() { stat -c '%a %u' "$1" 2>/dev/null || stat -f '%Lp %u' "$1" 2>/dev/null; }
cenv() { docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$1"; }
cname() { docker ps --filter "name=^${PROJECT}-$1-[0-9]+$" --format '{{.Names}}' | head -1; }
HSM_DIR=${HSM_DIR:-$(cd "$(dirname "$0")" && pwd)/hsm}
SWEEP_DIR=${SWEEP_DIR:-$(cd "$(dirname "$0")" && pwd)/sweep-config}
HSM_DIR=$(cd "$HSM_DIR" 2>/dev/null && pwd || echo "$HSM_DIR")
SWEEP_DIR=$(cd "$SWEEP_DIR" 2>/dev/null && pwd || echo "$SWEEP_DIR")
PKI_OUT=$(cd "$(dirname "$0")/../infra/pki" && pwd)/out

# One container's hardening flags (key-custody-hsm.md 5.6): read-only root, fixed uid, no capabilities, no-new-privileges, not privileged, own PID
# namespace and network (no host mode, no docker.sock), pids_limit, memory limit with memswap == memory, core dumps off, healthy.
#   hardening <container> <label> <uid> [<restart policy name:max-retries>]
hardening() {
	local c=$1 l=$2 uid=$3 rp=${4:-}
	local hc ro user capdrop secopt priv pidmode pids mem memswap restart ul netmode health mounts
	hc=$(docker inspect -f '{{.HostConfig.ReadonlyRootfs}}|{{.Config.User}}|{{.HostConfig.CapDrop}}|{{.HostConfig.SecurityOpt}}|{{.HostConfig.Privileged}}|{{.HostConfig.PidMode}}|{{.HostConfig.PidsLimit}}|{{.HostConfig.Memory}}|{{.HostConfig.MemorySwap}}|{{.HostConfig.RestartPolicy.Name}}:{{.HostConfig.RestartPolicy.MaximumRetryCount}}|{{range .HostConfig.Ulimits}}{{.Name}}={{.Soft}}/{{.Hard}} {{end}}|{{.HostConfig.NetworkMode}}' "$c")
	IFS='|' read -r ro user capdrop secopt priv pidmode pids mem memswap restart ul netmode <<<"$hc"
	[ "$ro" = true ] && ok "$l: read-only root filesystem" || bad "$l: root filesystem is writable"
	[ "$user" = "$uid:$uid" ] && ok "$l: runs as the fixed non-root uid $uid" || bad "$l: user is '$user', want $uid:$uid"
	case "$capdrop" in *ALL*) ok "$l: cap_drop ALL" ;; *) bad "$l: capabilities are not all dropped: $capdrop" ;; esac
	case "$secopt" in *no-new-privileges*) ok "$l: no-new-privileges" ;; *) bad "$l: no-new-privileges is not set: $secopt" ;; esac
	[ "$priv" = false ] && ok "$l: not privileged" || bad "$l: privileged container"
	[ "$pidmode" != host ] && [ "$netmode" != host ] && ok "$l: own PID and network namespaces" || bad "$l: pid or network mode is host"
	case "$pids" in ''|0|-1|'<no value>'|'<nil>') bad "$l: no pids_limit" ;; *) ok "$l: pids_limit $pids" ;; esac
	[ "${mem:-0}" -gt 0 ] && [ "$mem" = "$memswap" ] && ok "$l: memory limit $(( mem / 1048576 )) MiB, memswap == mem (no container swap)" || bad "$l: memory/memswap: $mem/$memswap (want equal and > 0)"
	case "$ul" in *core=0/0*) ok "$l: ulimit core 0 (no core dump)" ;; *) bad "$l: core dumps are not disabled: ${ul:-no ulimits}" ;; esac
	[ -n "$rp" ] && { [ "$restart" = "$rp" ] && ok "$l: restart policy $rp" || bad "$l: restart policy is '$restart', want $rp"; }
	mounts=$(docker inspect -f '{{range .Mounts}}{{.Source}} {{end}}' "$c")
	case "$mounts" in *docker.sock*) bad "$l: the docker socket is mounted" ;; *) ok "$l: no docker socket" ;; esac
	health=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$c")
	case "$health" in healthy) ok "$l: healthcheck healthy" ;; none) bad "$l: no healthcheck defined" ;; *) bad "$l: healthcheck is $health" ;; esac
}

case $NODE in
	vm2) PEER_PRIVATE=10.10.0.2; PEER_OPEN="9092 8545 8443 8080 5432 9100 8081 9187"; HAS_GW=1; HAS_CUSTODY=0 ;;
	vm3) PEER_PRIVATE=10.10.0.1; PEER_OPEN="5432 27017 9100 8081 9187"; HAS_GW=0; HAS_CUSTODY=1
		# hsm-gateway (key-custody-hsm.md M2b) listens on vm2's WireGuard address only when the Hot Wallet is on the HSM
		[ "${SIGNER_BACKEND:-software}" = pkcs11 ] && PEER_OPEN="$PEER_OPEN 8445" ;;
	vm4) PEER_PRIVATE=10.10.0.1; PEER_OPEN="5432 27017 9100 8081 9187"; HAS_GW=0; HAS_CUSTODY=0 ;;
	local) HAS_GW=1; HAS_CUSTODY=1 ;;
	*) echo "unknown node $NODE"; exit 2 ;;
esac
tcp() { timeout 3 bash -c "echo > /dev/tcp/$1/$2" 2>/dev/null; }
cd "$(dirname "$0")"

if [ "$NODE" != local ]; then
PEER_PUBLIC_IP=$(. /etc/digiwal/firewall.env && echo "${PEER_PUBLIC_IP%%,*}") # first peer; several on a three-node mesh
echo "1. WireGuard"
hs=$(wg show wg0 latest-handshakes | awk '{print $2}' | head -1)
if [ -n "$hs" ] && [ $(( $(date +%s) - hs )) -lt 180 ]; then ok "last handshake $(( $(date +%s) - hs ))s ago"; else bad "no recent handshake"; fi
if ping -c 2 -W 2 -M do -s 1372 "$PEER_PRIVATE" >/dev/null 2>&1; then ok "don't-fragment ping at MTU 1400 to $PEER_PRIVATE"; else bad "DF ping to $PEER_PRIVATE failed (MTU)"; fi

echo "2. The peer's public address exposes nothing but SSH (checked from here)"
for p in 5432 6379 9092 27017 8545 8084 8443 8444 8445 8446 8080 8085 9090 3000 9100 9101 9102 9187 8081 9308; do
	if tcp "$PEER_PUBLIC_IP" "$p"; then bad "port $p is open on the peer's public IP"; fi
done
tcp "$PEER_PUBLIC_IP" 22 && ok "ssh reachable, other service ports closed" || bad "ssh closed on the peer"

echo "3. The peer's private address serves what it should"
for p in $PEER_OPEN; do tcp "$PEER_PRIVATE" "$p" && ok "$PEER_PRIVATE:$p reachable" || bad "$PEER_PRIVATE:$p not reachable"; done
if [ "$NODE" = vm4 ]; then
	tcp "$PEER_PRIVATE" 8445 && bad "$PEER_PRIVATE:8445 (hsm-gateway) is reachable from vm4: only vm3 may call it" || ok "hsm-gateway ($PEER_PRIVATE:8445) is not reachable from vm4 (only vm3 may call it)"
fi
else
	echo "1-3. WireGuard and the peer's addresses"; skip "single host (NODE=local): there is no peer"
fi

echo "4. Containers on this node"
unhealthy=$(docker ps --filter "name=$PROJECT" --format '{{.Names}} {{.Status}}' | grep -Ei 'restarting|unhealthy|exited' || true)
n=$(docker ps --filter "name=$PROJECT" -q | wc -l | tr -d ' ')
[ -z "$unhealthy" ] && ok "$n containers up, none restarting or unhealthy" || bad "$unhealthy"
restarts=$(docker ps --filter "name=$PROJECT" -q | xargs -r docker inspect -f '{{.Name}} {{.RestartCount}}' | awk '$2>0')
[ -z "$restarts" ] && ok "no container has restarted" || note "restarts: $(echo $restarts)"

echo "5. Published ports are not on a public address"
if [ "$NODE" = local ]; then
	exposed=$(docker ps --filter "name=$PROJECT" --format '{{.Names}} {{.Ports}}' | grep -E "${PROJECT}-(hsm-gateway|sweep-signer|wallet-service)-" | grep -E '(^| )0\.0\.0\.0:|\[::\]:' || true)
	[ -z "$exposed" ] && ok "no custody container (hsm-gateway, sweep-signer, wallet-service) publishes on 0.0.0.0 or [::]" || bad "published on all interfaces: $exposed"
else
	exposed=$(docker ps --filter "name=$PROJECT" --format '{{.Names}} {{.Ports}}' | grep -E '(^| )0\.0\.0\.0:|\[::\]:' || true)
	[ -z "$exposed" ] && ok "no container publishes on 0.0.0.0 or [::]" || bad "published on all interfaces: $exposed"
fi

gw=$(cname hsm-gateway); ws=$(cname wallet-service); ss=$(cname sweep-signer)

if [ "${SIGNER_BACKEND:-software}" = pkcs11 ] && [ "$HAS_GW" = 1 ]; then
	echo "6. hsm-gateway (the Hot Key and the KEK live in its token)"
	if [ -z "$gw" ]; then bad "hsm-gateway container is not running"; else
		ok "hsm-gateway is running ($gw)"
		ports=$(docker port "$gw" 2>/dev/null)
		if [ "$NODE" = local ]; then
			[ -z "$ports" ] && ok "hsm-gateway publishes no port (single host: compose network only)" || bad "hsm-gateway publishes: $ports"
		else
			if [ -z "$ports" ] || [ "$(echo "$ports" | grep -vc '^8443/tcp -> 10\.10\.0\.1:8445$')" != 0 ]; then
				bad "hsm-gateway must publish exactly 10.10.0.1:8445 (WireGuard only): ${ports:-nothing}"
			else ok "hsm-gateway publishes only 10.10.0.1:8445 (the WireGuard address)"; fi
			tcp 10.10.0.1 8445 && ok "gateway answers on the WireGuard address" || bad "gateway does not answer on 10.10.0.1:8445"
			tcp 127.0.0.1 8445 && bad "gateway is reachable on loopback" || ok "gateway is not on loopback"
			if command -v ss >/dev/null 2>&1; then
				l=$(ss -H -ltn 'sport = :8445' | awk '{print $4}' | sort -u | tr '\n' ' ')
				[ "$l" = "10.10.0.1:8445 " ] && ok "the host listens on 8445 only at 10.10.0.1 (ss)" || bad "host listeners on 8445: '${l:-none}' (want only 10.10.0.1:8445)"
			else skip "host listener check (ss): ss is not installed on this host"; fi
		fi
		hardening "$gw" hsm-gateway 10001 on-failure:3
		genv=$(cenv "$gw")
		echo "$genv" | grep -qx 'HSM_PIN_FILE=/run/secrets/hsm-user-pin' && ok "hsm-gateway: the PIN is a file under /run/secrets (HSM_PIN_FILE), not a value" || bad "hsm-gateway: HSM_PIN_FILE is not /run/secrets/hsm-user-pin"
		gl=$(docker logs "$gw" 2>&1) # captured first: `docker logs | grep -q` under pipefail fails on SIGPIPE
		grep -q 'startup self-test passed' <<<"$gl" && ok "hsm-gateway: startup self-test passed" || bad "no 'startup self-test passed' line in hsm-gateway's log"
		grep -q 'ALERT HSM login refused' <<<"$gl" && bad "hsm-gateway logged a refused HSM login (wrong PIN or PIN guard)" || ok "no refused HSM login in hsm-gateway's log"
	fi
fi

if [ "${SIGNER_BACKEND:-software}" = pkcs11 ] && [ "$HAS_CUSTODY" = 1 ]; then
	echo "6b. wallet-service (Hot signing through the gateway, no key of its own)"
	if [ -z "$ws" ]; then bad "wallet-service container is not running"; else
		env_=$(cenv "$ws")
		echo "$env_" | grep -qx 'SIGNER_BACKEND=pkcs11' && ok "wallet-service runs with SIGNER_BACKEND=pkcs11" || bad "wallet-service is not on pkcs11"
		echo "$env_" | grep -q '^SOFT_' && bad "a SOFT_* variable is set on wallet-service" || ok "no SOFT_* variable on wallet-service"
		logs=$(docker logs "$ws" 2>&1)
		echo "$logs" | grep -q 'Hot Key: hsm-gateway at' && ok "wallet-service pinned the Hot address from hsm-gateway (the hot_wallet seed is not loaded)" || bad "no 'Hot Key: hsm-gateway' line in wallet-service's log"
		echo "$logs" | grep -q 'Hot Key: software' && bad "wallet-service loaded a software Hot Key since its last start" || ok "no software Hot Key was loaded"
		hardening "$ws" wallet-service 10003
	fi
fi

if [ "$HAS_CUSTODY" = 1 ]; then
	echo "7. sweep-signer: the only holder of the user_derivation seed (key-custody-hsm.md M3b, invariants 14-17)"
	if [ -z "$ss" ]; then bad "sweep-signer container is not running"; else
		ok "sweep-signer is running ($ss)"
		hardening "$ss" sweep-signer 10002 on-failure:5
		ul=$(docker inspect -f '{{range .HostConfig.Ulimits}}{{.Name}}={{.Soft}}/{{.Hard}} {{end}}' "$ss")
		case "$ul" in *memlock=*) ok "memlock limited in the compose file: $(echo $ul | tr ' ' '\n' | grep memlock)" ;; *) bad "no memlock ulimit (the Seed page's mlock needs a small RLIMIT_MEMLOCK)" ;; esac
		[ -z "$(docker port "$ss" 2>/dev/null)" ] && ok "sweep-signer publishes no port (reachable on the compose network only)" || bad "sweep-signer publishes: $(docker port "$ss")"
		if docker exec "$ss" sh -c true >/dev/null 2>&1; then bad "the sweep-signer image has a shell"; else ok "no shell in the sweep-signer image (docker exec sh fails)"; fi
		env_=$(cenv "$ss")
		echo "$env_" | grep -qE '^([A-Z_]*_PASSWORD|[A-Z_]*_KEY|[A-Z_]*_KEY_HEX|[A-Z_]*SECRET[A-Z_]*|[A-Z_]*MNEMONIC)=.+' && bad "a secret is an environment variable on sweep-signer" || ok "no secret in sweep-signer's environment"
		echo "$env_" | grep -q '^POSTGRES_PASSWORD_FILE=/run/secrets/' && ok "the database password is a secret FILE under /run/secrets" || bad "POSTGRES_PASSWORD_FILE is not a /run/secrets path"
		mounts=$(docker inspect -f '{{range .Mounts}}{{.Destination}} {{end}}' "$ss")
		case "$mounts" in *run/secrets/sweep-signer-db-password*) ok "only the database password file is mounted into sweep-signer (no key file: the seed comes from the HSM)" ;; *) bad "no db-password secret mount: $mounts" ;; esac
		case "$mounts" in *master*) bad "a master key file is mounted into sweep-signer: $mounts" ;; esac
		logs=$(docker logs "$ss" 2>&1)
		echo "$logs" | grep -q 'Hot address pinned from' && ok "pinned the Hot address ($(echo "$logs" | grep -m1 'Hot address pinned from' | sed 's/.*pinned from //' | cut -c1-60))" || bad "no 'Hot address pinned from' line in sweep-signer's log"
		if [ "${SIGNER_BACKEND:-software}" = pkcs11 ]; then
			echo "$logs" | grep -q 'Hot address pinned from hsm-gateway' && ok "the Hot address comes from hsm-gateway" || bad "with SIGNER_BACKEND=pkcs11 the Hot address must come from hsm-gateway"
		fi
		echo "$logs" | grep -qi 'ALERT' && note "ALERT lines in sweep-signer's log (refusals or an audit mismatch): $(echo "$logs" | grep -ci ALERT)" || ok "no ALERT in sweep-signer's log"
		# the limit really in force in the process (the compose ulimit is what was asked for; /proc says what the kernel applies)
		pid=$(docker inspect -f '{{.State.Pid}}' "$ss")
		if on_node && [ -r "/proc/$pid/limits" ]; then
			lim=$(awk '/Max locked memory/ {print $4, $5, $6}' "/proc/$pid/limits")
			case "$lim" in 65536\ 65536*) ok "RLIMIT_MEMLOCK in force for sweep-signer: 64 KiB soft and hard" ;; *) bad "RLIMIT_MEMLOCK of sweep-signer is '$lim', want 65536 65536 (/proc/$pid/limits)" ;; esac
		else skip "RLIMIT_MEMLOCK in force (/proc/<pid>/limits): $NEEDS_NODE"; fi
	fi
	if [ -n "$ws" ]; then
		wenv=$(cenv "$ws")
		echo "$wenv" | grep -E '^[A-Z_]*KEY_HEX=.+' >/dev/null && bad "wallet-service has an environment-held key" || ok "wallet-service holds no key (the seed is in the vault, opened by hsm-gateway for sweep-signer only)"
	fi
fi

echo "8. No key or PIN in any container environment, no key file mounted"
allc=$(docker ps --filter "name=$PROJECT" -q)
leaks=""
for c in $allc; do
	nm=$(docker inspect -f '{{.Name}}' "$c")
	e=$(cenv "$c")
	echo "$e" | grep -qE '^MASTER_KEY_HEX=.+' && leaks="$leaks $nm:MASTER_KEY_HEX"
	echo "$e" | grep -qE '^([A-Z0-9_]*_)?PIN=.+' && leaks="$leaks $nm:PIN"
	echo "$e" | grep -qE '^[A-Z0-9_]*(MNEMONIC|PRIVATE_KEY|SEED)[A-Z0-9_]*=.+' && leaks="$leaks $nm:mnemonic-or-key"
done
[ -z "$leaks" ] && ok "no MASTER_KEY_HEX, PIN, mnemonic or private key in the environment of any of $(echo $allc | wc -w | tr -d " ") containers (docker inspect)" || bad "secret in a container environment:$leaks"
if [ -r "$HSM_DIR/hsm-user-pin" ]; then
	pin=$(tr -d '[:space:]' < "$HSM_DIR/hsm-user-pin")
	found=""
	for c in $allc; do cenv "$c" | grep -qF -- "$pin" && found="$found $(docker inspect -f '{{.Name}}' "$c")"; done
	[ -z "$found" ] && ok "the PIN's value is in no container's environment" || bad "the PIN's value appears in the environment of:$found"
else skip "PIN value in container environments: $HSM_DIR/hsm-user-pin is not readable here (it lives on the gateway node)"; fi
keymounts=$(for c in $allc; do docker inspect -f '{{.Name}} {{range .Mounts}}{{.Source}} {{end}}' "$c"; done | grep -E 'master-key|master_key|ca-key\.pem' || true)
[ -z "$keymounts" ] && ok "no container mounts a master key file or the CA key" || bad "key file mounted: $keymounts"

echo "9. Key files, secrets and policy: modes and owners"
# modes are checked everywhere; OWNERS only on the node (as root: on a laptop the files are yours, Docker Desktop maps them for the container)
chk() { # <path> <want mode> <want uid> <label>
	local mu m u
	mu=$(mo "$1") || { bad "$4: $1 does not exist"; return; }
	m=${mu% *}; u=${mu#* }
	[ "$m" = "$2" ] && ok "$4: mode $m" || bad "$4: $1 has mode $m, want $2"
	if on_node; then [ "$u" = "$3" ] && ok "$4: owner uid $u" || bad "$4: $1 is owned by uid $u, want $3"; else skip "$4 owner (want uid $3): $NEEDS_NODE"; fi
}
nk=0
for d in "$PKI_OUT"/*/; do
	svc=$(basename "$d"); [ -f "$d/key.pem" ] || continue
	nk=$((nk+1)); u=$(../infra/pki/leaf-uid.sh "$svc" 2>/dev/null || echo 0)
	chk "$d/key.pem" 400 "$u" "leaf key $svc"
	mu=$(mo "${d%/}"); [ "${mu% *}" = 500 ] && ok "leaf directory $svc: mode 500" || bad "leaf directory $svc has mode ${mu% *}, want 500"
done
[ "$nk" -gt 0 ] || bad "no leaf key found under $PKI_OUT"
if [ "$HAS_GW" = 1 ]; then
	if [ -d "$HSM_DIR" ]; then
		chk "$HSM_DIR/hsm-user-pin" 400 10001 "HSM user PIN file"
		for f in policy.yaml callers.yaml; do
			if [ ! -e "$HSM_DIR/$f" ]; then bad "$HSM_DIR/$f does not exist"; continue; fi
			m=$(mo "$HSM_DIR/$f"); m=${m% *}
			[ $(( 0$m & 022 )) = 0 ] && ok "$f: mode $m (not group or world writable)" || bad "$f: mode $m is writable by group or others"
			if on_node; then [ "$(mo "$HSM_DIR/$f" | cut -d' ' -f2)" = 0 ] && ok "$f: owned by root" || bad "$f is not owned by root"; else skip "$f owner (want root): $NEEDS_NODE"; fi
		done
	else skip "HSM files: $HSM_DIR does not exist on this host (set HSM_DIR)"; fi
	if [ -n "$gw" ]; then
		genv=$(cenv "$gw")
		if [ "$NODE" = local ]; then skip "POLICY_REQUIRE_ROOT_OWNER=true: a laptop is not root, the gateway runs with the check off (dev profile)"
		else echo "$genv" | grep -qx 'POLICY_REQUIRE_ROOT_OWNER=true' && ok "hsm-gateway refuses a policy file that is not root-owned (POLICY_REQUIRE_ROOT_OWNER=true)" || bad "POLICY_REQUIRE_ROOT_OWNER is not true on hsm-gateway"; fi
		st=$(docker exec "$gw" stat -c '%a %u %n' /var/lib/softhsm/tokens /var/lib/hsm-gateway/audit /var/lib/hsm-gateway/state 2>&1)
		bad_st=$(echo "$st" | awk '($1!="700" && $1!="2700") || $2!="10001" {print}') # 2700: Debian's setgid directory bit
		[ -n "$st" ] && [ -z "$bad_st" ] && ok "token directory, audit directory and PIN-guard state are mode 700, owner uid 10001 (inside the container)" || bad "hsm-gateway state directories: ${bad_st:-$st} (want 700 and uid 10001)"
		mrw=$(docker inspect -f '{{range .Mounts}}{{.Destination}}={{.RW}} {{end}}' "$gw")
		case "$mrw" in *"/config/policy.yaml=false"*"/config/callers.yaml=false"*|*"/config/callers.yaml=false"*"/config/policy.yaml=false"*) ok "policy.yaml and callers.yaml are mounted read-only" ;; *) bad "policy/callers are not both read-only mounts: $mrw" ;; esac
		gwid=$(docker inspect -f '{{.Id}}' "$gw")
		other=$(for c in $allc; do [ "$(docker inspect -f '{{.Id}}' "$c")" = "$gwid" ] && continue; docker inspect -f '{{.Name}} {{range .Mounts}}{{.Source}} {{end}}' "$c"; done | grep -E "$HSM_DIR/(policy|callers)\.yaml|hsm-user-pin|hsm_audit|hsm_tokens|hsm_state" || true)
		[ -z "$other" ] && ok "no other container mounts the policy, the callers file, the PIN, the token or the audit log" || bad "mounted into another container: $other"
	fi
fi
if [ "$HAS_CUSTODY" = 1 ]; then
	if [ -d "$SWEEP_DIR" ]; then
		chk "$SWEEP_DIR/db-password" 400 10002 "sweep-signer database password file"
		chk "$SWEEP_DIR/keyadmin-db-password" 400 10004 "key-admin database password file"
	else skip "sweep-signer host files: $SWEEP_DIR does not exist on this host (set SWEEP_DIR)"; fi
fi
if [ "$NODE" = local ]; then skip "no CA key on this host: the operator machine holds it (a single host is also the operator)"
elif on_node; then
	ca=$(ls "${CA_DIR:-$HOME/.digiwal-ca}/ca-key.pem" 2>/dev/null; find /root /home /etc/digiwal /opt /srv -maxdepth 4 \( -name 'ca-key.pem' -o -name 'digiwal-ca' \) 2>/dev/null)
	[ -z "$ca" ] && ok "no CA key on this node (it lives on the operator machine)" || bad "a CA key is on this node: $(echo $ca): move it off (docs/runbooks/ca-key-off-vm2.md)"
else skip "no CA key on this node: $NEEDS_NODE"; fi
if git -C .. rev-parse --git-dir >/dev/null 2>&1; then
	miss=""
	for p in infra/pki/out/wallet-service/key.pem dev/hsm/hsm-user-pin dev/hsm/attest-pubkey dev/hsm/policy.yaml dev/sweep-config/db-password dev/sweep-config/keyadmin-db-password \
		e2e/hsm-config/hsm-user-pin e2e/sweep-config/db-password dev/.env e2e/.env ca-key.pem infra/pki/ca/ca-key.pem pin-guard.json audit.log; do
		git -C .. check-ignore -q "$p" || miss="$miss $p"
	done
	[ -z "$miss" ] && ok "every generated secret/PIN/key path is gitignored" || bad "not gitignored:$miss"
else skip "gitignore coverage: this directory is not a git checkout"; fi

echo "10. Vault schema: wallet-service's role is locked out (key-custody-hsm.md invariant 28)"
pg=$(cname postgres)
if [ "$HAS_CUSTODY" = 1 ] && [ -n "$pg" ]; then
	role=$(docker exec "$pg" psql -U custody -d custody -tAc "SELECT rolname FROM pg_roles WHERE rolname IN ('svc_wallet','e2e_wallet_probe') ORDER BY rolname DESC LIMIT 1" 2>/dev/null)
	if [ -z "$role" ]; then bad "neither svc_wallet nor the e2e probe role exists: the vault roles were not set up (make vault-roles)"; else
		priv=$(docker exec "$pg" psql -U custody -d custody -tAc "SELECT has_schema_privilege('$role','vault','USAGE'), has_table_privilege('$role','vault.sealed_secrets','SELECT'), has_table_privilege('$role','vault.sealed_secrets','INSERT,UPDATE,DELETE'), has_table_privilege('$role','vault.active_secrets','SELECT')" 2>&1)
		[ "$priv" = "f|f|f|f" ] && ok "role $role has no USAGE on schema vault and no access to vault.sealed_secrets or vault.active_secrets" || bad "role $role can reach the vault (usage|select|write|active view): $priv"
	fi
else skip "vault grants: there is no custody database on this node (NODE=$NODE)"; fi

echo "11. hsm-gateway audit chain"
if [ "$HAS_GW" = 1 ] && [ -n "$gw" ]; then
	out=$(docker exec "$gw" service audit-verify 2>&1)
	if echo "$out" | grep -q '^audit chain OK'; then
		seq=$(echo "$out" | sed -n 's/.*head_seq=\([0-9]*\).*/\1/p'); hash=$(echo "$out" | sed -n 's/.*head_hash=\([0-9a-f]*\).*/\1/p')
		ok "audit chain verifies: $(echo "$out" | sed 's/^audit chain OK //')"
		# against the head recorded by the previous run: it must still be in the log (a truncated or rewritten log loses it)
		last="$HSM_DIR/.verify-audit-head"
		if [ -r "$last" ]; then
			read -r pseq phash < "$last"
			if [ "$seq" -lt "$pseq" ]; then bad "audit log is SHORTER than at the last verify (head_seq $seq < $pseq): truncated"
			elif docker exec "$gw" grep -q "\"hash\":\"$phash\"" /var/lib/hsm-gateway/audit/audit.log; then ok "the head recorded at the last verify (seq $pseq) is still in the chain; the head advanced $((seq - pseq)) entries"
			else bad "the head recorded at the last verify (seq $pseq, $phash) is not in the log: it was rewritten"; fi
		else note "first run: the head is recorded now; the next run proves the log was only appended to"; fi
		[ -d "$HSM_DIR" ] && [ -w "$HSM_DIR" ] && printf '%s %s\n' "$seq" "$hash" > "$last"
	else bad "audit chain does not verify: $out"; fi
else skip "audit chain: hsm-gateway is not on this node (NODE=$NODE); run it on the gateway node"; fi

echo "12. Host settings the containers cannot give (swap, ptrace)"
if [ "$HAS_GW" = 1 ] || [ "$HAS_CUSTODY" = 1 ]; then
	if on_node; then
		ps=$(cat /proc/sys/kernel/yama/ptrace_scope 2>/dev/null || echo unknown)
		case "$ps" in 1|2|3) ok "kernel.yama.ptrace_scope=$ps" ;; unknown) bad "ptrace_scope is unreadable on this host (is Yama enabled?)" ;; *) bad "kernel.yama.ptrace_scope=$ps (want >= 1)" ;; esac
		[ "$(wc -l < /proc/swaps)" -le 1 ] && ok "swap is off on this host" || bad "swap is on: the PIN, the unwrapped keys and the Seed page could reach disk"
	else
		skip "kernel.yama.ptrace_scope >= 1: $NEEDS_NODE"
		skip "swap off: $NEEDS_NODE"
	fi
fi

if [ "$NODE" = vm3 ]; then
	echo "13. Monitoring (Prometheus on this node)"
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

if [ "$fail" = 0 ]; then
	[ "$skipped" = 0 ] && echo "RESULT: all checks passed" || echo "RESULT: all decidable checks passed ($skipped SKIPPED: not decidable on this host, see the SKIP lines)"
else echo "RESULT: FAILED"; fi
exit $fail
