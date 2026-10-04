#!/usr/bin/env bash
# QA stack lifecycle:  ./stack.sh up | down | reset
# State (CA, master key, mnemonics) lives in tests/e2e/.state (gitignored) — test material only.
set -euo pipefail
cd "$(dirname "$0")"
STATE="$PWD/.state"; mkdir -p "$STATE"
export CA_DIR="$STATE/ca"
C="docker compose -p digiwal-qa"

up() {
  [[ -f "$CA_DIR/ca-key.pem" ]] || ../../infra/pki/gen-ca.sh
  for s in wallet-service ledger-service withdrawal-service chain-watcher sweeper api-gateway evil-service; do
    [[ -f ../../infra/pki/out/$s/cert.pem ]] || ../../infra/pki/gen-leaf.sh "$s"
  done
  [[ -f "$STATE/master.env" ]] || echo "MASTER_KEY_HEX=$(openssl rand -hex 32)" > "$STATE/master.env"
  set -a; . "$STATE/master.env"; set +a
  $C up -d --build postgres redis kafka anvil
  until $C exec -T postgres pg_isready -U custody >/dev/null 2>&1; do sleep 1; done
  $C exec -T postgres psql -U custody -tc "SELECT 1 FROM pg_database WHERE datname='ledger'" | grep -q 1 \
    || $C exec -T postgres psql -U custody -c "CREATE DATABASE ledger"
  if [[ ! -f "$STATE/mnemonics.env" ]]; then
    : > "$STATE/mnemonics.env"
    for role in user_derivation hot_wallet; do
      m=$($C run --rm -T wallet-service -init-seed $role | grep -E '^([a-z]+ ){23}[a-z]+$')
      echo "MNEMONIC_${role}=\"$m\"" >> "$STATE/mnemonics.env"
    done
  fi
  # fund the hot wallet (anvil account #0 pays) before wallet-service starts reading its nonce
  # NOTE: pre-create the topic. Without it ledger-service's consumer never starts consuming (finding K-1,
  # reproduced by TestOPS_K1); the suite works around it so the other cases can run.
  until $C exec -T kafka /opt/kafka/bin/kafka-topics.sh --bootstrap-server localhost:9092 --create --if-not-exists --topic deposits.confirmed >/dev/null 2>&1; do sleep 2; done
  $C up -d --build wallet-service ledger-service
  until curl -sf localhost:18080/healthz >/dev/null && curl -sf localhost:18081/healthz >/dev/null; do sleep 1; done
  echo "stack up: wallet :18080/:18443 ledger :18081/:18444 anvil :18545 pg :15432 redis :16379 (kafka via docker exec)"
}
case "${1:-}" in
  up) up;;
  down) $C down -v;;
  reset) $C down -v; rm -rf "$STATE/mnemonics.env"; up;;
  *) echo "usage: $0 up|down|reset"; exit 2;;
esac
