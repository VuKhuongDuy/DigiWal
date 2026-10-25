#!/usr/bin/env bash
# Set the permissions of leaf directories (key-custody-hsm.md invariant 27): key.pem 0400, certs 0444, directory 0500, readable by the
# service's fixed uid only (infra/pki/leaf-uid.sh). As root it also chowns to that uid; as an ordinary user it leaves ownership alone.
#   infra/pki/fix-perms.sh <leaf-dir>...          e.g. infra/pki/out/wallet-service
#   LEAF_LOOSE=1 ...                              755/644 instead, for a throwaway local Linux stack run by a non-root user
# A directory with no fixed uid (an unknown service) is tightened but not chowned. Idempotent.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
[[ $# -ge 1 ]] || { echo "usage: $0 <leaf-dir>..." >&2; exit 2; }
for d in "$@"; do
  d="${d%/}"
  [[ -f "$d/key.pem" ]] || { echo "fix-perms: no key.pem in $d" >&2; exit 1; }
  chmod u+rwx "$d"
  if [[ "${LEAF_LOOSE:-0}" = 1 ]]; then
    chmod 755 "$d"; chmod 644 "$d"/*.pem
    continue
  fi
  chmod 444 "$d/cert.pem" "$d/ca-cert.pem"
  chmod 400 "$d/key.pem"
  if [[ "$(id -u)" = 0 ]] && uid="$("$HERE/leaf-uid.sh" "$(basename "$d")" 2>/dev/null)"; then chown -R "$uid:$uid" "$d"; fi
  chmod 500 "$d"
done
