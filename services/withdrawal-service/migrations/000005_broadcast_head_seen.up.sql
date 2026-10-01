-- Task 3.9: the Reaper (withdrawal-flow.md invariant #17) waits the Stuck Broadcast Timeout — 128
-- BLOCKS, not minutes — before it will act on a `broadcast` request. The pipeline records only
-- broadcast_at (a time), so the Reaper stamps the chain head the first time it sees the row. That
-- can only make it wait longer than 128 blocks after the broadcast, never shorter.
ALTER TABLE withdrawal_requests ADD COLUMN broadcast_head_seen BIGINT;
