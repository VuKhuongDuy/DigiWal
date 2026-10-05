-- The Postgres exporter's login (docs/design/multi-node-deployment.md, Monitoring wiring): pg_monitor reads the
-- statistics views and nothing else, and the session is read-only. Run as the superuser on each node's server, with
-- -v pw=<password>. Idempotent.
SELECT 'CREATE ROLE exporter LOGIN' WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'exporter') \gexec
ALTER ROLE exporter PASSWORD :'pw' NOSUPERUSER NOCREATEDB NOCREATEROLE;
GRANT pg_monitor TO exporter;
ALTER ROLE exporter SET default_transaction_read_only = on;
REVOKE CONNECT ON DATABASE postgres FROM PUBLIC;
GRANT CONNECT ON DATABASE postgres TO exporter;
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
