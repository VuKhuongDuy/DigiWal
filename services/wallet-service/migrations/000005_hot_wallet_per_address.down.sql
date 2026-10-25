-- Back to one nonce row per chain. Only possible while a chain has a single Hot address: with two (a
-- cutover happened) collapsing them would merge two nonce domains, so this refuses instead.
DO $$
BEGIN
	IF EXISTS (SELECT 1 FROM hot_wallet_nonces GROUP BY chain_id HAVING count(*) > 1) THEN
		RAISE EXCEPTION 'cannot downgrade: a chain has nonce rows for more than one Hot address';
	END IF;
END $$;

ALTER TABLE hot_wallet_signatures DROP CONSTRAINT hot_wallet_signatures_chain_hot_nonce_key;
ALTER TABLE hot_wallet_signatures ADD CONSTRAINT hot_wallet_signatures_chain_id_nonce_key UNIQUE (chain_id, nonce);
ALTER TABLE hot_wallet_signatures DROP COLUMN hot_address;

ALTER TABLE hot_wallet_nonces DROP CONSTRAINT hot_wallet_nonces_pkey;
ALTER TABLE hot_wallet_nonces DROP COLUMN hot_address;
ALTER TABLE hot_wallet_nonces ADD PRIMARY KEY (chain_id);
