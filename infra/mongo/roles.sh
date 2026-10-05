#!/bin/sh
# Run inside the mongo container (`docker exec -i mongo env ... sh -s < roles.sh`), idempotent.
#   ADMIN_PW    the new password of the root user `custody` (initially `custody`, set by MONGO_INITDB_*)
#   HISTORY_PW  password of `svc_history`, who may read and write the `history` database only
# Also `auditor_ro` (built-in `read` on `history`, nothing else; the auditor runs on the other node). DEV ONLY: a fixed
# password, like services/auditor/roles/mongo.js.
set -eu
: "${ADMIN_PW:?}" "${HISTORY_PW:?}"
can() { mongosh --quiet -u custody -p "$1" --authenticationDatabase admin --eval 'db.runCommand({ping:1}).ok' >/dev/null 2>&1; }
if can "$ADMIN_PW"; then
	pw=$ADMIN_PW
elif can custody; then
	mongosh --quiet -u custody -p custody --authenticationDatabase admin \
		--eval "db.getSiblingDB('admin').changeUserPassword('custody', '$ADMIN_PW')" >/dev/null
	pw=$ADMIN_PW
else
	echo "cannot authenticate to Mongo as custody with either password" >&2
	exit 1
fi
mongosh --quiet -u custody -p "$pw" --authenticationDatabase admin --eval "
	const h = db.getSiblingDB('history');
	if (h.getUser('svc_history')) { h.changeUserPassword('svc_history', '$HISTORY_PW'); }
	else { h.createUser({user: 'svc_history', pwd: '$HISTORY_PW', roles: [{role: 'readWrite', db: 'history'}]}); }
	if (!h.getUser('auditor_ro')) { h.createUser({user: 'auditor_ro', pwd: 'auditor_ro', roles: [{role: 'read', db: 'history'}]}); }
	print('mongo roles ok');"
