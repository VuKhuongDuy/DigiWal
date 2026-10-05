-- Grafana's login on the auditor's own database (docs/design/reconciliation-audit.md, Amendments 2): the ONE Postgres
-- datasource in Grafana, read-only, used by the Engineering dashboard only, for the detail tables (open suspicious
-- transactions, treasury movements, run history, reconciliation figures). DEV ONLY (fixed password, like the rest of
-- the dev environment). Run as a superuser against the auditor database, after the auditor has migrated. Idempotent.
--
-- It can read exactly four tables. Not pending_findings (an unconfirmed first sighting is not a finding yet) and not
-- audit_cursor (internal bookkeeping). It can write nothing.
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'grafana_ro') THEN
        CREATE ROLE grafana_ro LOGIN PASSWORD 'grafana_ro' NOSUPERUSER NOCREATEDB NOCREATEROLE;
    END IF;
    EXECUTE format('GRANT CONNECT ON DATABASE %I TO grafana_ro', current_database());
END $$;
ALTER ROLE grafana_ro SET default_transaction_read_only = on;
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM grafana_ro;
GRANT USAGE ON SCHEMA public TO grafana_ro;
GRANT SELECT ON suspicious_transactions, treasury_movements, job_runs, recon_snapshots TO grafana_ro;
