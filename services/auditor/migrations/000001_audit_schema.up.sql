-- docs/design/reconciliation-audit.md, Decision (b). The auditor's own database: the only place it writes.

CREATE TABLE suspicious_transactions (
    id             BIGSERIAL PRIMARY KEY,
    flow           TEXT NOT NULL CHECK (flow IN ('withdrawal','deposit','treasury','ledger','custody')),
    reference      TEXT NOT NULL,        -- withdrawal_id, or chain_id:tx_hash:log_index, or check scope
    rule           TEXT NOT NULL,        -- W1..W10, D1..D7, T1..T5, L1..L9, C1
    severity       TEXT NOT NULL CHECK (severity IN ('info','warn','critical')),
    status         TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','resolved','acknowledged')),
    detail         JSONB NOT NULL,       -- both sides of the mismatch, store by store
    first_run_id   BIGINT NOT NULL,
    last_run_id    BIGINT NOT NULL,
    detected_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at    TIMESTAMPTZ,
    note           TEXT,
    UNIQUE (flow, reference, rule)       -- invariant 5: one row per violated rule per transaction
);

CREATE TABLE job_runs (
    id          BIGSERIAL PRIMARY KEY,
    job         TEXT NOT NULL,
    started_at  TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ,
    result      TEXT CHECK (result IN ('ok','warn','fail','error')),
    findings    INT NOT NULL DEFAULT 0,
    error       TEXT,
    -- a run is finished exactly when it has a result; a crash leaves result NULL, which is never read as ok
    CHECK ((finished_at IS NULL) = (result IS NULL))
);
CREATE INDEX job_runs_job_started ON job_runs (job, started_at DESC);

-- every transfer with Hot, Warm or Cold on one side, found by scanning the chain
CREATE TABLE treasury_movements (
    chain_id     BIGINT NOT NULL,
    tx_hash      TEXT NOT NULL,
    log_index    INT NOT NULL,           -- -1 native
    asset        TEXT NOT NULL,
    amount       NUMERIC(78,0) NOT NULL,
    from_address TEXT NOT NULL,
    to_address   TEXT NOT NULL,
    from_tier    TEXT NOT NULL,          -- hot|warm|cold|deposit|user|external
    to_tier      TEXT NOT NULL,
    kind         TEXT NOT NULL,          -- hot_to_warm, warm_to_hot, warm_to_cold, cold_to_warm, hot_to_cold, cold_to_hot,
                                         -- sweep, gas_topup, withdrawal, deposit_into_tier, unexplained
    block_number BIGINT NOT NULL,
    block_time   TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (chain_id, tx_hash, log_index)
);

CREATE TABLE audit_cursor (
    job        TEXT PRIMARY KEY,
    last_block BIGINT,
    last_id    TEXT,
    updated_at TIMESTAMPTZ
);
