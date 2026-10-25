-- key-custody-hsm.md M3a, invariants 10-13: deposit addresses are derived by PUBLIC derivation from an Attested xpub, so the
-- address-issuing path holds no seed. One row per chain family.
--
--   chain        'evm' only. xpub issuance needs BIP32 public child derivation, which exists for secp256k1 (EVM, Bitcoin, Tron)
--                and not for ed25519: SLIP-10 ed25519 is hardened-only, so Solana cannot issue from an xpub (a new design, not an
--                extension; invariant 13).
--   path, depth  the account node m/44'/60'/0' (depth 3). Children are m/44'/60'/0'/0/{index}, the SAME tree wallet-service has
--                always used, so every existing derivation_index keeps its address.
--   xpub         the account-level extended public key. Never returned by an API and never logged.
--   attestation  raw r||s (64 bytes), ECDSA P-256 / SHA-256 by the HSM attestation key over the canonical statement
--                (chain, path, xpub). wallet-service verifies it on EVERY issuance against a public key pinned OUTSIDE this
--                database (XPUB_ATTEST_PUBKEY_FILE), so a database-only attacker cannot swap the xpub.
--   next_index   the atomic allocation counter for xpub issuance (it replaces wallet_seeds.next_index for this path, so issuing
--                never reads wallet_seeds). `key-admin migrate-xpub` sets it from wallet_seeds.next_index.
CREATE TABLE wallet_xpubs (
	chain           TEXT PRIMARY KEY CHECK (chain = 'evm'),
	path            TEXT NOT NULL CHECK (path = 'm/44''/60''/0'''),
	depth           SMALLINT NOT NULL CHECK (depth = 3),
	xpub            TEXT NOT NULL CHECK (xpub LIKE 'xpub%'),
	attestation     BYTEA NOT NULL CHECK (octet_length(attestation) = 64),
	attestation_alg TEXT NOT NULL DEFAULT 'ecdsa-p256-sha256' CHECK (attestation_alg = 'ecdsa-p256-sha256'),
	next_index      INT NOT NULL CHECK (next_index >= 0),
	created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
