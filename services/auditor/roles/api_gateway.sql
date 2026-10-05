-- Read-only grants for auditor_ro on this database (A2). Column-level where a table also holds something the
-- auditor must not read. Must match source.Contract exactly (contract_test.go checks both directions).
-- Apply as a superuser or the database owner, after 00_role.sql and after the service has migrated.
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM auditor_ro;
GRANT USAGE ON SCHEMA public TO auditor_ro;
-- NOT granted: api_keys (key hashes).
GRANT SELECT ON users TO auditor_ro;
