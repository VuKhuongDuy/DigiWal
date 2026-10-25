-- key-custody-hsm.md 5.5, M2c: the Hot cutover (old software Hot address -> new HSM Hot address).
--
-- hot_cutover is the operator-only freeze and the cutover's state machine, one row per (chain, old Hot address). While a
-- row's state is not 'done', SignHotWallet allocates no new Hot nonce for the chain (503 hot_cutover): the freeze is
-- chain-wide, so it holds across the restart that switches wallet-service from the old key to the HSM key. Nothing over
-- HTTP lifts it; only the operator tool (cmd/hot-cutover) moves the state, like the Nonce Gap precedent.
--   frozen    signing frozen, the old Hot key is still the Hot key. Aborting (back to unfrozen) is possible.
--   draining  POINT OF NO RETURN: the drain transactions are signed and stored (hot_wallet_signatures + hot_cutover_drains).
--   drained   every drain tx is mined and its gas is in the ledger; the Hot address may now be switched.
--   done      unfrozen. The row stays as the record of the cutover.
CREATE TABLE hot_cutover (
	chain_id  BIGINT NOT NULL,
	old_hot   TEXT NOT NULL,
	new_hot   TEXT NOT NULL CHECK (new_hot <> old_hot),
	state     TEXT NOT NULL CHECK (state IN ('frozen', 'draining', 'drained', 'done')),
	frozen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	ponr_at   TIMESTAMPTZ,
	done_at   TIMESTAMPTZ,
	PRIMARY KEY (chain_id, old_hot)
);
-- At most one open cutover per chain: two freezes could be lifted by either.
CREATE UNIQUE INDEX hot_cutover_one_open ON hot_cutover (chain_id) WHERE state <> 'done';

-- One drain transaction of the old Hot address. The signed tx itself is a hot_wallet_signatures row (same idempotency key,
-- the old address's dense nonce sequence stays intact, invariant 24). asset_id is the Asset Registry id; round is 1
-- unless funds arrived on the old address after a round completed (a late Sweep): then the tool plans round+1.
CREATE TABLE hot_cutover_drains (
	chain_id            BIGINT NOT NULL,
	old_hot             TEXT NOT NULL,
	asset_id            TEXT NOT NULL,
	round               INT NOT NULL CHECK (round >= 1),
	idempotency_key     TEXT NOT NULL UNIQUE REFERENCES hot_wallet_signatures (idempotency_key),
	tx_hash             TEXT NOT NULL,
	nonce               BIGINT NOT NULL,
	amount              NUMERIC(78,0) NOT NULL CHECK (amount > 0),
	broadcast_at        TIMESTAMPTZ,
	confirmed_at        TIMESTAMPTZ,
	gas_used            BIGINT,
	effective_gas_price NUMERIC(78,0),
	gas_posted_at       TIMESTAMPTZ,
	PRIMARY KEY (chain_id, old_hot, asset_id, round),
	FOREIGN KEY (chain_id, old_hot) REFERENCES hot_cutover (chain_id, old_hot)
);
