#!/usr/bin/env bash
# The fixed uid each service's container runs as (key-custody-hsm.md invariant 27), so its leaf private key can be readable by that uid ONLY.
#   infra/pki/leaf-uid.sh <service-name>      prints the uid; exit 1 for a name with no entry
# One table, used by gen-leaf.sh (modes) and by the push-certs targets (chown on the node). Keep it equal to the `user:` of the service in the compose files
# and the Dockerfiles. hot-cutover runs as wallet-service (it presents wallet-service's leaf to hsm-gateway).
case "${1:-}" in
  hsm-gateway)        echo 10001 ;;
  sweep-signer)       echo 10002 ;;
  wallet-service|hot-cutover) echo 10003 ;;
  key-admin)          echo 10004 ;;
  ledger-service)     echo 10010 ;;
  api-gateway)        echo 10011 ;;
  withdrawal-service) echo 10012 ;;
  chain-watcher)      echo 10013 ;;
  sweeper)            echo 10014 ;;
  auditor)            echo 10015 ;;
  *) echo "leaf-uid.sh: no fixed uid for '${1:-}'" >&2; exit 1 ;;
esac
