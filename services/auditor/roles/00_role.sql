-- The auditor's read-only login (docs/design/reconciliation-audit.md, A2). DEV ONLY: a fixed password, like
-- the rest of the dev environment. Roles are cluster-wide, so this runs once per Postgres server; the
-- per-database grants are in the other files here. Idempotent.
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'auditor_ro') THEN
        CREATE ROLE auditor_ro LOGIN PASSWORD 'auditor_ro' NOSUPERUSER NOCREATEDB NOCREATEROLE;
    END IF;
END $$;
ALTER ROLE auditor_ro SET default_transaction_read_only = on;
