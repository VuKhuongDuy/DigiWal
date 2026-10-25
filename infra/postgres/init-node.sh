#!/bin/sh
# Creates one login role and one database per entry of PG_DB_ROLES ("<database>=<role> ..."): the role owns its
# database, CONNECT is revoked from PUBLIC, and the role's password is read from the variable PW_<role>.
# Roles are created here, not by the services' migrations, so a service never needs more than its own database.
#
# Idempotent. It runs by itself when a node's data directory is first created (docker-entrypoint-initdb.d), and
# `make roles` runs it again inside the container so a database added to PG_DB_ROLES later (the auditor's) appears on
# a node that already has data.
set -eu
for pair in $PG_DB_ROLES; do
	db=${pair%%=*}
	role=${pair##*=}
	eval "pw=\${PW_$role:?PW_$role is not set}"
	psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres <<SQL
SELECT 'CREATE ROLE $role LOGIN' WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '$role') \gexec
ALTER ROLE $role PASSWORD '$pw';
SELECT 'CREATE DATABASE $db OWNER $role' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = '$db') \gexec
REVOKE CONNECT ON DATABASE $db FROM PUBLIC;
GRANT CONNECT ON DATABASE $db TO $role;
SQL
done
