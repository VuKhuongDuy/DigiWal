package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"time"
)

// JobStatus is what the dashboards need to know about one job (docs/design/reconciliation-audit.md invariants 2 and 7):
// when it last finished and with what, kept apart from when it last succeeded, so a job that keeps failing or
// erroring can never look healthy.
type JobStatus struct {
	Job          string
	LastRunAt    time.Time // finished_at of the latest finished run
	LastResult   string    // ok | warn | fail | error
	LastDuration time.Duration
	LastSuccess  *time.Time // finished_at of the latest run that ended ok or warn; nil if never
}

// OpenCount is open suspicious transactions of one flow, rule and severity.
type OpenCount struct {
	Flow, Rule, Severity string
	Count                int64
}

// ReconFigures are the latest reconciliation figures of one asset, whole asset units.
type ReconFigures struct {
	Asset                                       string
	OnChain, LedgerCustody, Explained, Residual float64
}

// Metrics is what the auditor exports, read from its own database.
type Metrics struct {
	Jobs          []JobStatus
	Open          []OpenCount
	OldestOpenAge map[string]float64 // by severity, seconds, 0 when none
	Detected      []OpenCount        // every finding ever recorded (rows are never deleted: monotonic)
	Recon         []ReconFigures
}

func (s *Store) Metrics(ctx context.Context) (Metrics, error) {
	m := Metrics{OldestOpenAge: map[string]float64{"info": 0, "warn": 0, "critical": 0}}

	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (job) job, finished_at, result, extract(epoch FROM finished_at - started_at)
		FROM job_runs WHERE finished_at IS NOT NULL ORDER BY job, finished_at DESC`)
	if err != nil {
		return m, fmt.Errorf("job runs: %w", err)
	}
	byJob := map[string]int{}
	for rows.Next() {
		var j JobStatus
		var secs float64
		if err := rows.Scan(&j.Job, &j.LastRunAt, &j.LastResult, &secs); err != nil {
			rows.Close()
			return m, err
		}
		j.LastDuration = time.Duration(secs * float64(time.Second))
		byJob[j.Job] = len(m.Jobs)
		m.Jobs = append(m.Jobs, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return m, err
	}
	rows, err = s.pool.Query(ctx, `SELECT job, max(finished_at) FROM job_runs WHERE result IN ('ok', 'warn') GROUP BY job`)
	if err != nil {
		return m, fmt.Errorf("job successes: %w", err)
	}
	for rows.Next() {
		var job string
		var at time.Time
		if err := rows.Scan(&job, &at); err != nil {
			rows.Close()
			return m, err
		}
		if i, ok := byJob[job]; ok {
			m.Jobs[i].LastSuccess = &at
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return m, err
	}

	counts := func(sql string) ([]OpenCount, error) {
		r, err := s.pool.Query(ctx, sql)
		if err != nil {
			return nil, err
		}
		defer r.Close()
		var out []OpenCount
		for r.Next() {
			var c OpenCount
			if err := r.Scan(&c.Flow, &c.Rule, &c.Severity, &c.Count); err != nil {
				return nil, err
			}
			out = append(out, c)
		}
		return out, r.Err()
	}
	if m.Open, err = counts(`SELECT flow, rule, severity, count(*) FROM suspicious_transactions WHERE status = 'open' GROUP BY 1, 2, 3`); err != nil {
		return m, fmt.Errorf("open findings: %w", err)
	}
	if m.Detected, err = counts(`SELECT flow, rule, severity, count(*) FROM suspicious_transactions GROUP BY 1, 2, 3`); err != nil {
		return m, fmt.Errorf("detected findings: %w", err)
	}
	ages, err := s.pool.Query(ctx, `SELECT severity, extract(epoch FROM now() - min(detected_at)) FROM suspicious_transactions WHERE status = 'open' GROUP BY severity`)
	if err != nil {
		return m, fmt.Errorf("oldest open: %w", err)
	}
	for ages.Next() {
		var sev string
		var age float64
		if err := ages.Scan(&sev, &age); err != nil {
			ages.Close()
			return m, err
		}
		m.OldestOpenAge[sev] = age
	}
	ages.Close()
	if err := ages.Err(); err != nil {
		return m, err
	}

	rs, err := s.pool.Query(ctx, `SELECT DISTINCT ON (asset_id) symbol, on_chain::text, ledger_custody::text, explained::text, residual::text, components
		FROM recon_snapshots ORDER BY asset_id, id DESC`)
	if err != nil {
		return m, fmt.Errorf("reconciliation figures: %w", err)
	}
	defer rs.Close()
	for rs.Next() {
		var f ReconFigures
		var onChain, ledger, explained, residual string
		var raw []byte
		if err := rs.Scan(&f.Asset, &onChain, &ledger, &explained, &residual, &raw); err != nil {
			return m, err
		}
		var comp map[string]string
		if err := json.Unmarshal(raw, &comp); err != nil {
			return m, err
		}
		dec, ok := parseDecimals(comp["decimals"])
		if !ok { // a snapshot from before decimals were recorded cannot be shown in whole units
			continue
		}
		f.OnChain, f.LedgerCustody, f.Explained, f.Residual = whole(onChain, dec), whole(ledger, dec), whole(explained, dec), whole(residual, dec)
		m.Recon = append(m.Recon, f)
	}
	return m, rs.Err()
}

func parseDecimals(s string) (int, bool) {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n < 0 || n > 77 {
		return 0, false
	}
	return n, true
}

// whole turns a minor-unit decimal string into whole asset units for display only.
func whole(minor string, decimals int) float64 {
	n, ok := new(big.Int).SetString(minor, 10)
	if !ok {
		return 0
	}
	f := new(big.Float).SetInt(n)
	f.Quo(f, new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)))
	v, _ := f.Float64()
	return v
}
