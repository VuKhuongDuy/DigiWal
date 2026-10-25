#!/usr/bin/env bash
# Pin the xpub attestation PUBLIC key outside the database (key-custody-hsm.md invariant 10, M3a).
#   infra/hsm/pin-attest-key.sh <config-dir>   < output of `hsm-gateway init-token` (or init-attest-key)
# Reads the `attest_public_key=<hex>` line from stdin, checks it is the hex of a 65-byte uncompressed point and writes
# <config-dir>/attest-pubkey (public, mode 644). wallet-service reads it as XPUB_ATTEST_PUBKEY_FILE and `key-admin migrate-xpub`
# compares it with the key the gateway reports. An existing, different pin is never overwritten silently: delete it on purpose.
set -euo pipefail
DIR="${1:?usage: pin-attest-key.sh <config-dir> < init-token output}"
line="$(sed -n 's/^attest_public_key=//p' | head -1)"
[[ "$line" =~ ^04[0-9a-f]{128}$ ]] || { echo "no attest_public_key=04<128 hex> line on stdin (is the gateway image from M3a or later?)" >&2; exit 1; }
mkdir -p "$DIR"
if [[ -f "$DIR/attest-pubkey" ]] && [[ "$(tr -d '[:space:]' < "$DIR/attest-pubkey")" != "$line" ]]; then
  echo "$DIR/attest-pubkey already pins a different key: refusing to replace it (delete it only if you mean to re-pin)" >&2
  exit 1
fi
(umask 022 && printf '%s\n' "$line" > "$DIR/attest-pubkey")
echo "pinned attestation key $DIR/attest-pubkey (${line:0:18}...)"
