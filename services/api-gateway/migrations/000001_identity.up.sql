-- Identity (docs/design/identity-auth.md): api-gateway's own Postgres.

-- A User: a system-generated id, never client-chosen (invariant #1).
CREATE TABLE users (
	id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- An API Key belongs to one user. Only its SHA-256 is stored, never the key (invariant #2).
-- Revocation is a timestamp checked on every request (invariant #6): no blocklist.
CREATE TABLE api_keys (
	id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	user_id    UUID NOT NULL REFERENCES users (id),
	key_hash   BYTEA NOT NULL UNIQUE CHECK (octet_length(key_hash) = 32),
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	revoked_at TIMESTAMPTZ
);
CREATE INDEX api_keys_user ON api_keys (user_id);
