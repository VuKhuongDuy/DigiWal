#!/usr/bin/env bash
# One-time (per environment) generation of the project CA — see
# docs/design/service-auth-mtls.md. ECDSA P-256, CA cert valid 10 years.
#
# The CA *key* is written to $CA_DIR (default ~/.digiwal-ca), deliberately
# outside the repo and outside anything docker-compose mounts
# (service-auth-mtls.md invariant #5). Only the public ca-cert.pem is ever
# copied next to leaf certs by gen-leaf.sh.
set -euo pipefail

CA_DIR="${CA_DIR:-$HOME/.digiwal-ca}"

if [[ -e "$CA_DIR/ca-key.pem" ]]; then
  echo "CA already exists in $CA_DIR — refusing to overwrite (regenerating means re-issuing every leaf cert)." >&2
  exit 1
fi

umask 077
mkdir -p "$CA_DIR"
openssl ecparam -name prime256v1 -genkey -noout -out "$CA_DIR/ca-key.pem"
openssl req -new -x509 -sha256 -days 3650 \
  -key "$CA_DIR/ca-key.pem" -out "$CA_DIR/ca-cert.pem" \
  -subj "/CN=digiwal-internal-ca"
chmod 644 "$CA_DIR/ca-cert.pem"

echo "CA created in $CA_DIR (keep ca-key.pem off the repo and off every container)."
