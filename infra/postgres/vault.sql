-- The vault schema (key-custody-hsm.md invariants 18, 19 and 28, M3c): Sealed Secrets at rest and the two database roles that may touch them.
-- Run as the database SUPERUSER (never as wallet-service's role), in the custody database, once per environment and again after any change
-- here. Idempotent. Order: infra/postgres/sweep-signer-role.sql first (it creates svc_sweepsigner), then this file. The key-admin password
-- arrives as a psql variable from a secret FILE:
--   psql -U custody -d custody -v ON_ERROR_STOP=1 -v pw_keyadmin="$(cat key-admin-db-password)" -f infra/postgres/vault.sql
--
-- WHO CAN DO WHAT
--   svc_keyadmin   (key-admin, one-off ceremonies)  INSERT and SELECT on vault.sealed_secrets, UPDATE of status only, DELETE of pending rows only
--                  (both enforced by the guard trigger below, not by convention). It cannot change a ciphertext, a wrapped DEK or an AAD.
--   svc_sweepsigner (sweep-signer)                  SELECT on the view vault.active_secrets: the ACTIVE row of each role, nothing else. Cannot write.
--   everybody else, wallet-service included          NO access: no USAGE on the schema, no grant on any object in it, PUBLIC revoked. wallet-service's
--                                                    role owns the custody database, not the vault schema (this file is run by the superuser, which
--                                                    owns the schema), so database ownership does not reach it.
CREATE SCHEMA IF NOT EXISTS vault;
REVOKE ALL ON SCHEMA vault FROM PUBLIC;

CREATE TABLE IF NOT EXISTS vault.sealed_secrets (
    role         text        NOT NULL,
    version      integer     NOT NULL CHECK (version >= 1),
    status       text        NOT NULL CHECK (status IN ('pending', 'active', 'retired')),
    purpose      text        NOT NULL,
    kek_label    text        NOT NULL,
    kek_version  integer     NOT NULL CHECK (kek_version >= 1),
    wrapped_dek  bytea       NOT NULL,
    gcm_nonce    bytea       NOT NULL CHECK (octet_length(gcm_nonce) = 12),
    ciphertext   bytea       NOT NULL,
    aad          bytea       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (role, version),
    CHECK (kek_label = 'kek-v' || kek_version)
);
-- at most one active version per role (invariant 19)
CREATE UNIQUE INDEX IF NOT EXISTS sealed_secrets_one_active ON vault.sealed_secrets (role) WHERE status = 'active';

-- Only a status may change, only along pending -> active, active -> retired and retired -> active (rollback); only a pending row may be deleted.
CREATE OR REPLACE FUNCTION vault.sealed_secrets_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.status <> 'pending' THEN
            RAISE EXCEPTION 'vault.sealed_secrets: only a pending row may be deleted (this one is %)', OLD.status USING ERRCODE = 'insufficient_privilege';
        END IF;
        RETURN OLD;
    END IF;
    IF (NEW.role, NEW.version, NEW.purpose, NEW.kek_label, NEW.kek_version, NEW.wrapped_dek, NEW.gcm_nonce, NEW.ciphertext, NEW.aad, NEW.created_at)
       IS DISTINCT FROM
       (OLD.role, OLD.version, OLD.purpose, OLD.kek_label, OLD.kek_version, OLD.wrapped_dek, OLD.gcm_nonce, OLD.ciphertext, OLD.aad, OLD.created_at) THEN
        RAISE EXCEPTION 'vault.sealed_secrets: only status may be updated' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF NOT ((OLD.status, NEW.status) IN (('pending', 'active'), ('active', 'retired'), ('retired', 'active'), ('pending', 'pending'), ('active', 'active'), ('retired', 'retired'))) THEN
        RAISE EXCEPTION 'vault.sealed_secrets: status % -> % is not allowed', OLD.status, NEW.status USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS sealed_secrets_guard ON vault.sealed_secrets;
CREATE TRIGGER sealed_secrets_guard BEFORE UPDATE OR DELETE ON vault.sealed_secrets FOR EACH ROW EXECUTE FUNCTION vault.sealed_secrets_guard();
-- TRUNCATE is not row-level: it is simply never granted.

-- what sweep-signer reads: the active row of each role, and only the columns the gateway needs back
CREATE OR REPLACE VIEW vault.active_secrets AS
    SELECT role, version, purpose, kek_label, kek_version, wrapped_dek, gcm_nonce, ciphertext, aad
      FROM vault.sealed_secrets WHERE status = 'active';

-- the password goes through a session setting, so the statements below are plain SQL (no psql-only gexec: the test suite runs this file through a driver)
SELECT set_config('vault.pw_keyadmin', :'pw_keyadmin', false);
DO $$ BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'svc_keyadmin') THEN
        EXECUTE format('CREATE ROLE svc_keyadmin LOGIN PASSWORD %L', current_setting('vault.pw_keyadmin'));
    END IF;
    EXECUTE format('ALTER ROLE svc_keyadmin LOGIN PASSWORD %L NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS', current_setting('vault.pw_keyadmin'));
END $$;
DO $$ BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'svc_sweepsigner') THEN
        RAISE EXCEPTION 'svc_sweepsigner does not exist: run infra/postgres/sweep-signer-role.sql first';
    END IF;
END $$;

GRANT CONNECT ON DATABASE custody TO svc_keyadmin;
REVOKE ALL ON ALL TABLES IN SCHEMA vault FROM PUBLIC, svc_keyadmin, svc_sweepsigner;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA vault FROM PUBLIC;
GRANT USAGE ON SCHEMA vault TO svc_keyadmin, svc_sweepsigner;
GRANT SELECT, INSERT, DELETE ON vault.sealed_secrets TO svc_keyadmin;
GRANT UPDATE (status) ON vault.sealed_secrets TO svc_keyadmin;
GRANT SELECT ON vault.active_secrets TO svc_sweepsigner;
