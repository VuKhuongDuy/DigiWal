-- Invariant 4 (two-pass): a mismatch seen once is remembered here, and only becomes a
-- suspicious_transactions row when the next run sees it again. In the database, not in memory, so a
-- restart between the two runs does not reset the count.
CREATE TABLE pending_findings (
    flow         TEXT NOT NULL,
    reference    TEXT NOT NULL,
    rule         TEXT NOT NULL,
    severity     TEXT NOT NULL CHECK (severity IN ('info','warn','critical')),
    detail       JSONB NOT NULL,
    first_run_id BIGINT NOT NULL,
    seen_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (flow, reference, rule)
);
