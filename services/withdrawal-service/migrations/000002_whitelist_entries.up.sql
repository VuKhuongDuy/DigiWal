-- Withdrawal Whitelist (docs/design/withdrawal-flow.md invariants #12, #13, #15). An entry is
-- `pending` until Whitelist Cooldown has passed since added_at, then `active`. That status is
-- computed on read, never stored, so no background job can be late and widen the window.
-- Removal is a DELETE: no state is kept for that direction.
CREATE TABLE whitelist_entries (
    id       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id  TEXT NOT NULL,
    address  TEXT NOT NULL CHECK (address ~ '^0x[0-9a-fA-F]{40}$'), -- EIP-55 form, one per address
    added_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, address)
);
