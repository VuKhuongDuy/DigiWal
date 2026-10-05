#!/bin/sh
# Runs once, when a node's Postgres data directory is first created (docker-entrypoint-initdb.d).
# Creates one login role and one database per entry of PG_DB_ROLES ("<database>=<role> ..."): the role owns its
# database, CONNECT is revoked from PUBLIC, and the role's password is read from the variable PW_<role>.
# Roles are created here, not by the services' migrations, so a service never needs more than its own database.
set -eu
for pair in $PG_DB_ROLES; do
	db=${pair%%=*}
	role=${pair##*=}
	eval "pw=\${PW_$role:?PW_$role is not set}"
	psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres <<SQL
CREATE ROLE $role LOGIN PASSWORD '$pw';
CREATE DATABASE $db OWNER $role;
REVOKE CONNECT ON DATABASE $db FROM PUBLIC;
GRANT CONNECT ON DATABASE $db TO $role;
SQL
done
