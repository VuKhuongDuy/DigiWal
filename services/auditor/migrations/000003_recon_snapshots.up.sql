-- The reconciliation's figures per asset per run (docs/design/reconciliation-audit.md, "Aggregate reconciliation"),
-- so the residual can be plotted over time and is visible while it is within tolerance, not only when it is
-- violated. Amounts are minor units; residual and ledger_custody may be negative.
CREATE TABLE recon_snapshots (
    id             BIGSERIAL PRIMARY KEY,
    run_id         BIGINT NOT NULL,
    asset_id       TEXT NOT NULL,
    symbol         TEXT NOT NULL,
    block_number   BIGINT NOT NULL,
    on_chain       NUMERIC(78,0) NOT NULL,
    ledger_custody NUMERIC(78,0) NOT NULL,
    explained      NUMERIC(78,0) NOT NULL,
    residual       NUMERIC(78,0) NOT NULL,
    tolerance      NUMERIC(78,0) NOT NULL,
    components     JSONB NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX recon_snapshots_asset_time ON recon_snapshots (asset_id, created_at DESC);
