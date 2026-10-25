package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The Hot cutover's state (key-custody-hsm.md 5.5, migration 000006). Everything here is reachable only from the operator
// tool cmd/hot-cutover; nothing is on an HTTP route.

// Cutover states, in order.
const (
	CutoverFrozen   = "frozen"   // signing frozen; the old key is still the Hot key; abort is possible
	CutoverDraining = "draining" // POINT OF NO RETURN: the drain txs are signed and stored
	CutoverDrained  = "drained"  // every drain tx is mined and its gas is in the ledger
	CutoverDone     = "done"     // unfrozen
)

var (
	// ErrHotCutover: a Hot cutover is open on the chain, so no new Hot nonce is allocated (HTTP 503 hot_cutover).
	// An already-stored idempotency key still replays, like under a Nonce Gap.
	ErrHotCutover = errors.New("hot cutover in progress: hot wallet signing frozen")
	// ErrCutoverOpen: another cutover (a different old Hot address) is already open on the chain.
	ErrCutoverOpen = errors.New("another hot cutover is open on this chain")
	// ErrCutoverMismatch: the cutover row names a different new Hot address than the one asked for.
	ErrCutoverMismatch = errors.New("the open cutover targets a different new Hot address")
	// ErrCutoverState: the operation is not allowed in the cutover's current state.
	ErrCutoverState = errors.New("not allowed in this cutover state")
)

// Cutover is one row of hot_cutover.
type Cutover struct {
	ChainID  int64
	OldHot   string // lower-case hex
	NewHot   string
	State    string
	FrozenAt time.Time
	PonrAt   *time.Time
	DoneAt   *time.Time
}

func scanCutover(row pgx.Row) (Cutover, error) {
	var c Cutover
	err := row.Scan(&c.ChainID, &c.OldHot, &c.NewHot, &c.State, &c.FrozenAt, &c.PonrAt, &c.DoneAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Cutover{}, ErrNotFound
	}
	return c, err
}

const cutoverCols = `chain_id, old_hot, new_hot, state, frozen_at, ponr_at, done_at`

// GetCutover returns the cutover of (chain, old Hot address), ErrNotFound if none.
func (s *Store) GetCutover(ctx context.Context, chainID int64, old string) (Cutover, error) {
	return scanCutover(s.pool.QueryRow(ctx, `SELECT `+cutoverCols+` FROM hot_cutover WHERE chain_id = $1 AND old_hot = $2`, chainID, strings.ToLower(old)))
}

// OpenCutover returns the chain's cutover that is not done, ErrNotFound if signing is not frozen by one.
func (s *Store) OpenCutover(ctx context.Context, chainID int64) (Cutover, error) {
	return scanCutover(s.pool.QueryRow(ctx, `SELECT `+cutoverCols+` FROM hot_cutover WHERE chain_id = $1 AND state <> 'done'`, chainID))
}

// FreezeHotSigning opens the cutover (step 2): from the commit on, SignHotWallet allocates no new Hot nonce on the chain.
// It first takes the old address's nonce-row lock, the one every signer holds, so a sign that was already in flight
// finishes (and is in next_nonce) before the freeze is visible, and none starts after it. Idempotent: the same
// (old, new) returns the existing row in whatever state it is.
func (s *Store) FreezeHotSigning(ctx context.Context, chainID int64, old, newHot string) (Cutover, error) {
	old, newHot = strings.ToLower(old), strings.ToLower(newHot)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Cutover{}, fmt.Errorf("beginning tx: %w", err)
	}
	defer tx.Rollback(ctx)
	var n int64
	if err := tx.QueryRow(ctx, `SELECT next_nonce FROM hot_wallet_nonces WHERE chain_id = $1 AND hot_address = $2 FOR UPDATE`, chainID, old).Scan(&n); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Cutover{}, fmt.Errorf("locking the old Hot nonce row: %w", err)
	}
	if existing, err := scanCutover(tx.QueryRow(ctx, `SELECT `+cutoverCols+` FROM hot_cutover WHERE chain_id = $1 AND old_hot = $2`, chainID, old)); err == nil {
		if existing.NewHot != newHot {
			return Cutover{}, fmt.Errorf("%w: %s (asked for %s)", ErrCutoverMismatch, existing.NewHot, newHot)
		}
		return existing, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Cutover{}, err
	}
	c, err := scanCutover(tx.QueryRow(ctx, `INSERT INTO hot_cutover (chain_id, old_hot, new_hot, state) VALUES ($1, $2, $3, 'frozen') RETURNING `+cutoverCols, chainID, old, newHot))
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return Cutover{}, ErrCutoverOpen
	}
	if err != nil {
		return Cutover{}, fmt.Errorf("opening the cutover: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Cutover{}, fmt.Errorf("committing tx: %w", err)
	}
	return c, nil
}

// AbortCutover lifts the freeze and forgets the cutover. Only while it is 'frozen': once the drain is planned (the
// point of no return) it is ErrCutoverState. No cutover row is a no-op (idempotent).
func (s *Store) AbortCutover(ctx context.Context, chainID int64, old string) error {
	old = strings.ToLower(old)
	tag, err := s.pool.Exec(ctx, `DELETE FROM hot_cutover WHERE chain_id = $1 AND old_hot = $2 AND state = 'frozen'`, chainID, old)
	if err != nil {
		return fmt.Errorf("aborting the cutover: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	c, err := s.GetCutover(ctx, chainID, old)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: the cutover is %s, past the point of no return (only forward)", ErrCutoverState, c.State)
}

// SetCutoverState moves draining -> drained and drained -> done (and is a no-op when already there). It also moves
// frozen -> drained, for an old Hot address that was already empty: nothing was signed, so nothing was drained.
func (s *Store) SetCutoverState(ctx context.Context, chainID int64, old, state string) error {
	old = strings.ToLower(old)
	from := map[string][]string{CutoverDrained: {CutoverDraining, CutoverFrozen}, CutoverDone: {CutoverDrained}}[state]
	if from == nil {
		return fmt.Errorf("%w: cannot move to %q", ErrCutoverState, state)
	}
	q := `UPDATE hot_cutover SET state = $3 WHERE chain_id = $1 AND old_hot = $2 AND state = ANY($4)`
	if state == CutoverDone {
		q = `UPDATE hot_cutover SET state = $3, done_at = now() WHERE chain_id = $1 AND old_hot = $2 AND state = ANY($4)`
	}
	tag, err := s.pool.Exec(ctx, q, chainID, old, state, from)
	if err != nil {
		return fmt.Errorf("moving the cutover to %s: %w", state, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	c, err := s.GetCutover(ctx, chainID, old)
	if err != nil {
		return err
	}
	if c.State == state {
		return nil
	}
	return fmt.Errorf("%w: the cutover is %s, cannot move to %s", ErrCutoverState, c.State, state)
}

// NonceRow is the old Hot address's counter, ok=false if it never signed anything.
func (s *Store) NonceRow(ctx context.Context, chainID int64, hot string) (next uint64, frozenFrom *uint64, ok bool, err error) {
	var n int64
	var f *int64
	err = s.pool.QueryRow(ctx, `SELECT next_nonce, frozen_from_nonce FROM hot_wallet_nonces WHERE chain_id = $1 AND hot_address = $2`, chainID, strings.ToLower(hot)).Scan(&n, &f)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, false, nil
	}
	if err != nil {
		return 0, nil, false, err
	}
	if f != nil {
		u := uint64(*f)
		frozenFrom = &u
	}
	return uint64(n), frozenFrom, true, nil
}

// HotTx is one signed key of a Hot address and every version of it (the original first, then each replacement in order).
type HotTx struct {
	Key      string
	Nonce    uint64
	CallerCN string
	Versions [][]byte
}

// HotTxsFrom lists the keys signed by hot at nonces >= fromNonce, by nonce. These are the ones the chain may not have
// mined yet; settle rebroadcasts their latest version.
func (s *Store) HotTxsFrom(ctx context.Context, chainID int64, hot string, fromNonce uint64) ([]HotTx, error) {
	rows, err := s.pool.Query(ctx, `SELECT idempotency_key, nonce, COALESCE(caller_cn, ''), signed_raw_tx FROM hot_wallet_signatures
		WHERE chain_id = $1 AND hot_address = $2 AND nonce >= $3 ORDER BY nonce`, chainID, strings.ToLower(hot), int64(fromNonce))
	if err != nil {
		return nil, fmt.Errorf("reading signatures: %w", err)
	}
	var out []HotTx
	for rows.Next() {
		var t HotTx
		var n int64
		var orig []byte
		if err := rows.Scan(&t.Key, &n, &t.CallerCN, &orig); err != nil {
			rows.Close()
			return nil, err
		}
		t.Nonce = uint64(n)
		t.Versions = [][]byte{orig}
		out = append(out, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		reps, err := lookupReplacements(ctx, s.pool, out[i].Key)
		if err != nil {
			return nil, fmt.Errorf("reading replacements: %w", err)
		}
		for _, r := range reps {
			out[i].Versions = append(out[i].Versions, r.SignedRawTx)
		}
	}
	return out, nil
}

// DrainPlan is one planned drain transaction, signed by the old software key.
type DrainPlan struct {
	Key     string
	AssetID string
	Round   int
	Amount  *big.Int
	Raw     []byte
	Hash    string
}

// Drain is one hot_cutover_drains row joined with its signed tx.
type Drain struct {
	Key               string
	AssetID           string
	Round             int
	Nonce             uint64
	TxHash            string
	Amount            *big.Int
	Raw               []byte
	BroadcastAt       *time.Time
	ConfirmedAt       *time.Time
	GasUsed           uint64
	EffectiveGasPrice *big.Int
	GasPostedAt       *time.Time
}

// DrainKey is the idempotency key (hot_wallet_signatures) of one drain tx: unique per cutover, asset and round.
func DrainKey(chainID int64, old, assetID string, round int) string {
	o := strings.TrimPrefix(strings.ToLower(old), "0x")
	if len(o) > 8 {
		o = o[:8]
	}
	return fmt.Sprintf("hot-cutover:%d:%s:%s:%d", chainID, o, strings.ToLower(assetID), round)
}

// PlanDrain is the point of no return. Under the old address's nonce-row lock it asks build to sign the drain txs
// at consecutive nonces starting at the row's next_nonce (the row is seeded from seedNonce if the old address never
// signed), stores each as a hot_wallet_signatures row plus a hot_cutover_drains row, advances next_nonce past them
// (the old address's sequence stays dense, so the auditor's per-address nonce checks hold) and moves the cutover to
// 'draining'. All or nothing. If any of the keys is already stored the plan is a replay and nothing changes.
// Returns false in that case.
func (s *Store) PlanDrain(ctx context.Context, chainID int64, old string, seedNonce func(context.Context) (uint64, error),
	build func(firstNonce uint64) ([]DrainPlan, error)) (planned bool, err error) {
	old = strings.ToLower(old)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("beginning tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM hot_cutover WHERE chain_id = $1 AND old_hot = $2 FOR UPDATE`, chainID, old).Scan(&state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, fmt.Errorf("%w: signing is not frozen (run freeze first)", ErrCutoverState)
		}
		return false, err
	}
	if state == CutoverDone {
		return false, fmt.Errorf("%w: the cutover is done", ErrCutoverState)
	}
	next, _, err := lockNonceRow(ctx, tx, chainID, old, seedNonce)
	if err != nil {
		return false, err
	}
	plans, err := build(uint64(next))
	if err != nil {
		return false, err
	}
	if len(plans) == 0 {
		return false, nil
	}
	for _, p := range plans {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM hot_wallet_signatures WHERE idempotency_key = $1)`, p.Key).Scan(&exists); err != nil {
			return false, err
		}
		if exists {
			return false, nil
		}
	}
	for i, p := range plans {
		nonce := next + int64(i)
		sum := sha256.Sum256([]byte(p.Hash))
		if _, err := tx.Exec(ctx, `INSERT INTO hot_wallet_signatures (idempotency_key, chain_id, hot_address, nonce, request_hash, signed_raw_tx, caller_cn)
			VALUES ($1, $2, $3, $4, $5, $6, 'hot-cutover')`, p.Key, chainID, old, nonce, hex.EncodeToString(sum[:]), p.Raw); err != nil {
			return false, fmt.Errorf("storing drain signature: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO hot_cutover_drains (chain_id, old_hot, asset_id, round, idempotency_key, tx_hash, nonce, amount)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8::numeric)`, chainID, old, p.AssetID, p.Round, p.Key, strings.ToLower(p.Hash), nonce, p.Amount.String()); err != nil {
			return false, fmt.Errorf("storing drain: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE hot_wallet_nonces SET next_nonce = next_nonce + $3 WHERE chain_id = $1 AND hot_address = $2`, chainID, old, int64(len(plans))); err != nil {
		return false, fmt.Errorf("advancing nonce: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE hot_cutover SET state = 'draining', ponr_at = COALESCE(ponr_at, now()) WHERE chain_id = $1 AND old_hot = $2`, chainID, old); err != nil {
		return false, fmt.Errorf("moving the cutover to draining: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("committing tx: %w", err)
	}
	return true, nil
}

// Drains lists the cutover's drain txs by (round, nonce).
func (s *Store) Drains(ctx context.Context, chainID int64, old string) ([]Drain, error) {
	rows, err := s.pool.Query(ctx, `SELECT d.idempotency_key, d.asset_id, d.round, d.nonce, d.tx_hash, d.amount::text, g.signed_raw_tx,
		d.broadcast_at, d.confirmed_at, COALESCE(d.gas_used, 0), COALESCE(d.effective_gas_price::text, ''), d.gas_posted_at
		FROM hot_cutover_drains d JOIN hot_wallet_signatures g ON g.idempotency_key = d.idempotency_key
		WHERE d.chain_id = $1 AND d.old_hot = $2 ORDER BY d.round, d.nonce`, chainID, strings.ToLower(old))
	if err != nil {
		return nil, fmt.Errorf("reading drains: %w", err)
	}
	defer rows.Close()
	var out []Drain
	for rows.Next() {
		var d Drain
		var n int64
		var amount, price string
		var gas int64
		if err := rows.Scan(&d.Key, &d.AssetID, &d.Round, &n, &d.TxHash, &amount, &d.Raw, &d.BroadcastAt, &d.ConfirmedAt, &gas, &price, &d.GasPostedAt); err != nil {
			return nil, err
		}
		d.Nonce, d.GasUsed = uint64(n), uint64(gas)
		d.Amount, _ = new(big.Int).SetString(amount, 10)
		if price != "" {
			d.EffectiveGasPrice, _ = new(big.Int).SetString(price, 10)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// MarkDrainBroadcast, MarkDrainConfirmed and MarkDrainGasPosted record progress; each is idempotent.
func (s *Store) MarkDrainBroadcast(ctx context.Context, key string) error {
	_, err := s.pool.Exec(ctx, `UPDATE hot_cutover_drains SET broadcast_at = COALESCE(broadcast_at, now()) WHERE idempotency_key = $1`, key)
	return err
}

func (s *Store) MarkDrainConfirmed(ctx context.Context, key string, gasUsed uint64, price *big.Int) error {
	_, err := s.pool.Exec(ctx, `UPDATE hot_cutover_drains SET confirmed_at = COALESCE(confirmed_at, now()), gas_used = $2, effective_gas_price = $3::numeric
		WHERE idempotency_key = $1`, key, int64(gasUsed), price.String())
	return err
}

func (s *Store) MarkDrainGasPosted(ctx context.Context, key string) error {
	_, err := s.pool.Exec(ctx, `UPDATE hot_cutover_drains SET gas_posted_at = COALESCE(gas_posted_at, now()) WHERE idempotency_key = $1`, key)
	return err
}
