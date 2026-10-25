-- Rolling back must not let the seed-based path reuse an index xpub issuance already handed out: carry the higher counter back
-- into wallet_seeds before dropping the table. (The user_derivation row exists whenever an xpub row does: migrate-xpub needs it.)
UPDATE wallet_seeds s
SET next_index = GREATEST(s.next_index, x.next_index)
FROM wallet_xpubs x
WHERE s.seed_role = 'user_derivation' AND x.chain = 'evm';
DROP TABLE wallet_xpubs;
