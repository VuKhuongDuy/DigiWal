#!/usr/bin/env bash
# Issue a leaf certificate for one internal service, signed by the project CA.
#   infra/pki/gen-leaf.sh <service-name>
# CN = service name (service-auth-mtls.md invariant #6), SAN DNS = the same
# name (the docker-compose network alias) plus localhost for local runs.
# Output: infra/pki/out/<service-name>/{cert.pem,key.pem,ca-cert.pem}
# (gitignored). Leaf certs are valid 2 years. Re-running rotates the cert.
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <service-name>" >&2
  exit 2
fi
SERVICE="$1"
if [[ ! "$SERVICE" =~ ^[a-z][a-z0-9-]*$ ]]; then
  echo "invalid service name: $SERVICE" >&2
  exit 2
fi

CA_DIR="${CA_DIR:-$HOME/.digiwal-ca}"
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="$HERE/out/$SERVICE"

[[ -f "$CA_DIR/ca-key.pem" ]] || { echo "no CA in $CA_DIR — run infra/pki/gen-ca.sh first" >&2; exit 1; }

umask 077
mkdir -p "$OUT"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

openssl ecparam -name prime256v1 -genkey -noout -out "$OUT/key.pem"
openssl req -new -key "$OUT/key.pem" -out "$TMP/leaf.csr" -subj "/CN=$SERVICE"
cat > "$TMP/ext.cnf" <<EXT
basicConstraints = critical, CA:FALSE
keyUsage = critical, digitalSignature
extendedKeyUsage = serverAuth, clientAuth
subjectAltName = DNS:$SERVICE, DNS:localhost
EXT
openssl x509 -req -sha256 -days 730 -in "$TMP/leaf.csr" \
  -CA "$CA_DIR/ca-cert.pem" -CAkey "$CA_DIR/ca-key.pem" -CAcreateserial \
  -CAserial "$TMP/ca.srl" -extfile "$TMP/ext.cnf" -out "$OUT/cert.pem" 2>/dev/null

cp "$CA_DIR/ca-cert.pem" "$OUT/ca-cert.pem"
# Containers run as a non-root user and read these through a read-only bind mount.
# The directory needs o+x too (mkdir ran under umask 077), or the container's
# non-root user gets "permission denied" traversing it.
chmod 755 "$OUT"
chmod 644 "$OUT/cert.pem" "$OUT/ca-cert.pem" "$OUT/key.pem"

echo "issued $SERVICE → $OUT"
