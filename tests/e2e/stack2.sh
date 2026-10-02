#!/usr/bin/env bash
# QA chaos stack (all 7 real services, Dev's compose + port shift). ./stack2.sh up|down|ps|logs <svc>
set -euo pipefail
cd "$(dirname "$0")"
STATE="$PWD/.state"; mkdir -p "$STATE"; export CA_DIR="$STATE/ca"
export MASTER_KEY_HEX=0000000000000000000000000000000000000000000000000000000000000001
C="docker compose -p digiwal-qa --project-directory ../../e2e -f ../../infra/docker-compose.yml -f ../../e2e/docker-compose.yml -f $PWD/ports.override.yml"
SERVICES="wallet-service ledger-service chain-watcher sweeper api-gateway withdrawal-service"
case "${1:-}" in
 up)
  [[ -f "$CA_DIR/ca-key.pem" ]] || ../../infra/pki/gen-ca.sh
  for s in $SERVICES evil-service; do [[ -f ../../infra/pki/out/$s/cert.pem ]] || ../../infra/pki/gen-leaf.sh "$s"; done
  $C up -d --wait postgres redis kafka mongo anvil
  for t in deposits.detected deposits.confirmed withdrawals.broadcast withdrawals.confirmed withdrawals.failed; do
    $C exec -T kafka /opt/kafka/bin/kafka-topics.sh --bootstrap-server localhost:9092 --create --if-not-exists --topic $t --partitions 1 --replication-factor 1 >/dev/null
  done
  $C run --rm wallet-service -init-seed user_derivation | grep -E '^([a-z]+ ){23}[a-z]+$' | sed 's/^/MNEMONIC_user_derivation=/' > "$STATE/mn2.env"
  $C run --rm wallet-service -init-seed hot_wallet | grep -E '^([a-z]+ ){23}[a-z]+$' | sed 's/^/MNEMONIC_hot_wallet=/' >> "$STATE/mn2.env"
  $C up -d --build --wait $SERVICES history-service
  $C ps --format '{{.Name}} {{.Status}}';;
 down) $C down -v --remove-orphans;;
 ps) $C ps --format '{{.Name}} {{.Status}}';;
 logs) shift; $C logs --tail 50 "$@";;
 *) echo "usage: $0 up|down|ps|logs <svc>"; exit 2;;
esac
