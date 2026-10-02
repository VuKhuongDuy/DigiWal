-- Task 3.14 (withdrawal-flow.md invariant #25): a destination that made a withdrawal revert through
-- no fault of ours is blocked on that user's entry, so no further withdrawal to it can burn gas.
-- `blocked` is stored (unlike pending/active, which are a function of time) and wins over both.
-- The only way back is to delete the entry and re-add it: a new row, a new 24h cooldown.
ALTER TABLE whitelist_entries ADD COLUMN blocked_at TIMESTAMPTZ;
