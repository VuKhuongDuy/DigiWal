#!/usr/bin/env bash
# Prepare the host-side files hsm-gateway needs in the DEV and e2e environments (never production: the PIN here is a
# random value kept in a plain file, which is what a dev SoftHSM token deserves and nothing more).
#   infra/hsm/prepare.sh <config-dir> [caller ...]      default callers: wallet-service key-admin sweep-signer
# Creates <config-dir>/hsm-user-pin (once; random, never printed; 0400, owner uid 10001 when run as root) and <config-dir>/callers.yaml (always: pins the leaf
# certificates as they are now). <config-dir> is gitignored; policy.yaml is written by write-policy.sh after the ceremony.
set -euo pipefail
DIR="${1:?usage: prepare.sh <config-dir> [caller ...]}"; shift || true
CALLERS=("$@"); [[ ${#CALLERS[@]} -gt 0 ]] || CALLERS=(wallet-service key-admin sweep-signer)
HERE="$(cd "$(dirname "$0")" && pwd)"

mkdir -p "$DIR"
chmod 755 "$DIR"
if [[ ! -f "$DIR/hsm-user-pin" ]]; then
  (umask 077 && openssl rand -hex 8 > "$DIR/hsm-user-pin")
  echo "wrote $DIR/hsm-user-pin"
fi
# The PIN is readable by hsm-gateway's uid ONLY (invariant 26/27): mode 0400, owner 10001 (needs root to chown: on a node it is root; a
# developer's Docker Desktop maps the owner for bind mounts, so the file stays yours and the container can still read it).
chmod 400 "$DIR/hsm-user-pin"
if [[ "$(id -u)" = 0 ]]; then chown 10001:10001 "$DIR/hsm-user-pin"; fi
# callers.yaml and policy.yaml are not secrets but they ARE the gate: 0644 and owned by whoever ran this (root on a node), never by uid 10001.
"$HERE/../pki/gen-callers.sh" "$DIR/callers.yaml" "${CALLERS[@]}"
