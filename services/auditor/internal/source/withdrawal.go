package source

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	"digiwal/services/auditor/internal/rules/withdrawal"

	"github.com/jackc/pgx/v5"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// HistoryReader is the read side of history-service's MongoDB (*Mongo implements it).
type HistoryReader interface {
	FindOne(ctx context.Context, collection string, id any) (bson.M, error)
}

// Withdrawals reads everything the W rules need, straight from each store.
type Withdrawals struct {
	Withdrawal, Ledger, Wallet Querier
	History                    HistoryReader
	Chain                      Chain
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Ref is a position in a scan: rows are visited in (At, ID) order.
type Ref struct {
	ID string
	At time.Time
}

// ListFinal returns up to limit requests in a final status whose last change is after `after` and at or
// before notAfter (now minus the Grace Period), oldest first.
func (w *Withdrawals) ListFinal(ctx context.Context, after Ref, notAfter time.Time, limit int) ([]Ref, error) {
	afterID := after.ID
	if afterID == "" {
		afterID = "00000000-0000-0000-0000-000000000000"
	}
	rows, err := w.Withdrawal.Query(ctx, `
		SELECT withdrawal_id::text, updated_at FROM withdrawal_requests
		WHERE status IN ('confirmed','failed','rejected')
		  AND (updated_at, withdrawal_id) > ($1, $2::uuid) AND updated_at <= $3
		ORDER BY updated_at, withdrawal_id LIMIT $4`, after.At, afterID, notAfter, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Ref
	for rows.Next() {
		var r Ref
		if err := rows.Scan(&r.ID, &r.At); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListSettles returns up to limit withdrawal_settle transactions in the ledger after `after`, no newer than
// notAfter; ID is the withdrawal id the Settle's key names. It finds Settles whose request is missing.
func (w *Withdrawals) ListSettles(ctx context.Context, after Ref, notAfter time.Time, limit int) ([]Ref, error) {
	rows, err := w.Ledger.Query(ctx, `
		SELECT idempotency_key, created_at FROM ledger_transactions
		WHERE source_type = 'withdrawal_settle' AND (created_at, idempotency_key) > ($1, $2) AND created_at <= $3
		ORDER BY created_at, idempotency_key LIMIT $4`, after.At, after.ID, notAfter, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Ref
	for rows.Next() {
		var r Ref
		if err := rows.Scan(&r.ID, &r.At); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Gather reads one withdrawal from every store. id is a withdrawal id, or the Settle key
// ("withdrawal:<id>") when called for an orphan. A request that does not exist gives Facts.Req == nil.
func (w *Withdrawals) Gather(ctx context.Context, id string) (withdrawal.Facts, error) {
	id = strings.TrimPrefix(id, "withdrawal:")
	key := "withdrawal:" + id
	var f withdrawal.Facts

	if uuidRe.MatchString(id) {
		req, err := w.request(ctx, id)
		if err != nil {
			return f, err
		}
		f.Req = req
	}
	if err := w.ledgerFacts(ctx, key, &f); err != nil {
		return f, err
	}
	if f.Req == nil {
		return f, nil
	}
	r := f.Req

	var err error
	if f.Sig, err = w.signature(ctx, key); err != nil {
		return f, err
	}
	if f.History, err = w.history(ctx, key); err != nil {
		return f, err
	}
	if f.Asset, err = w.asset(ctx, r.AssetID); err != nil {
		return f, err
	}
	if r.ChainID != nil && r.TxHash != "" {
		if f.GasFeePurpose, err = w.gasFee(ctx, *r.ChainID, r.TxHash); err != nil {
			return f, err
		}
	}

	hashes := map[string]bool{}
	if r.TxHash != "" {
		hashes[strings.ToLower(r.TxHash)] = true
	}
	for _, h := range r.VersionTxHashes {
		hashes[strings.ToLower(h)] = true
	}
	f.Chain = map[string]withdrawal.ChainTx{}
	var list []string
	for h := range hashes {
		list = append(list, h)
		c, err := w.Chain.Tx(ctx, h)
		if err != nil {
			return f, err
		}
		f.Chain[h] = c
	}
	if len(list) > 0 {
		if f.SharedBy, err = w.sharedBy(ctx, id, list); err != nil {
			return f, err
		}
	}
	return f, nil
}

func parseBig(s string) *big.Int {
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil
	}
	return n
}

func (w *Withdrawals) request(ctx context.Context, id string) (*withdrawal.Request, error) {
	var (
		r                    withdrawal.Request
		amount               string
		rejectReason, txHash *string
		settleTx, gasPrice   *string
	)
	err := w.Withdrawal.QueryRow(ctx, `
		SELECT withdrawal_id::text, user_id, asset_id, amount::text, to_address, status, reject_reason, tx_hash,
		       nonce, chain_id, confirmed_block, settle_ledger_tx_id, gas_price::text, updated_at
		FROM withdrawal_requests WHERE withdrawal_id = $1::uuid`, id).
		Scan(&r.ID, &r.UserID, &r.AssetID, &amount, &r.ToAddress, &r.Status, &rejectReason, &txHash,
			&r.Nonce, &r.ChainID, &r.ConfirmedBlock, &settleTx, &gasPrice, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("withdrawal request %s: %w", id, err)
	}
	r.Amount = parseBig(amount)
	r.RejectReason, r.TxHash = deref(rejectReason), deref(txHash)
	r.SettleLedgerTxID = deref(settleTx)
	if gasPrice != nil {
		r.GasPrice = parseBig(*gasPrice)
	}

	rows, err := w.Withdrawal.Query(ctx, `SELECT tx_hash FROM withdrawal_txs WHERE withdrawal_id = $1::uuid ORDER BY replacement_seq`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		r.VersionTxHashes = append(r.VersionTxHashes, h)
	}
	return &r, rows.Err()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (w *Withdrawals) ledgerFacts(ctx context.Context, key string, f *withdrawal.Facts) error {
	var lock withdrawal.Lock
	var amount string
	err := w.Ledger.QueryRow(ctx, `
		SELECT l.status, l.amount::text, a.asset_id::text, a.user_id
		FROM locks l JOIN accounts a ON a.id = l.account_id WHERE l.idempotency_key = $1`, key).
		Scan(&lock.Status, &amount, &lock.AssetID, &lock.UserID)
	switch {
	case err == nil:
		lock.Amount = parseBig(amount)
		f.Lock = &lock
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("lock %s: %w", key, err)
	}

	var settleID string
	err = w.Ledger.QueryRow(ctx, `SELECT id::text FROM ledger_transactions WHERE idempotency_key = $1 AND source_type = 'withdrawal_settle'`, key).Scan(&settleID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("settle %s: %w", key, err)
	}
	s := &withdrawal.Settle{ID: settleID}
	rows, err := w.Ledger.Query(ctx, `
		SELECT e.id::text, e.amount::text, a.kind, a.asset_id::text
		FROM entries e JOIN accounts a ON a.id = e.account_id WHERE e.ledger_transaction_id = $1::uuid`, settleID)
	if err != nil {
		return err
	}
	var entryIDs []string
	for rows.Next() {
		var eid, amt string
		var e withdrawal.Entry
		if err := rows.Scan(&eid, &amt, &e.Kind, &e.AssetID); err != nil {
			rows.Close()
			return err
		}
		e.Amount = parseBig(amt)
		s.Entries = append(s.Entries, e)
		entryIDs = append(entryIDs, eid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	f.Settle = s
	return w.Ledger.QueryRow(ctx, `SELECT count(DISTINCT ledger_transaction_id) FROM entries WHERE corrects_entry_id = ANY($1::uuid[])`, entryIDs).Scan(&f.Refunds)
}

func (w *Withdrawals) signature(ctx context.Context, key string) (*withdrawal.Signature, error) {
	var s withdrawal.Signature
	var cn *string
	err := w.Wallet.QueryRow(ctx, `SELECT nonce, caller_cn FROM hot_wallet_signatures WHERE idempotency_key = $1`, key).Scan(&s.Nonce, &cn)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("signature %s: %w", key, err)
	}
	s.CallerCN = deref(cn)
	return &s, nil
}

func (w *Withdrawals) history(ctx context.Context, key string) (*withdrawal.History, error) {
	doc, err := w.History.FindOne(ctx, "history", key)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("history %s: %w", key, err)
	}
	s := func(k string) string { v, _ := doc[k].(string); return v }
	return &withdrawal.History{Status: s("status"), TxHash: s("tx_hash"), Amount: s("amount"), AssetID: s("asset_id")}, nil
}

func (w *Withdrawals) asset(ctx context.Context, assetID string) (*withdrawal.Asset, error) {
	if !uuidRe.MatchString(assetID) {
		return nil, nil
	}
	var a withdrawal.Asset
	var contract *string
	err := w.Ledger.QueryRow(ctx, `SELECT asset_type, contract_address FROM assets WHERE id = $1::uuid`, assetID).Scan(&a.Type, &contract)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("asset %s: %w", assetID, err)
	}
	a.ContractAddress = deref(contract)
	return &a, nil
}

func (w *Withdrawals) gasFee(ctx context.Context, chainID int64, txHash string) (string, error) {
	var purpose *string
	err := w.Ledger.QueryRow(ctx, `SELECT purpose FROM ledger_transactions WHERE idempotency_key = $1 AND source_type = 'gas_fee'`,
		fmt.Sprintf("gas:%d:%s", chainID, strings.ToLower(txHash))).Scan(&purpose)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return deref(purpose), err
}

func (w *Withdrawals) sharedBy(ctx context.Context, id string, hashes []string) ([]string, error) {
	rows, err := w.Withdrawal.Query(ctx, `
		SELECT withdrawal_id::text FROM withdrawal_requests WHERE withdrawal_id <> $1::uuid AND lower(tx_hash) = ANY($2)
		UNION
		SELECT withdrawal_id::text FROM withdrawal_txs WHERE withdrawal_id <> $1::uuid AND lower(tx_hash) = ANY($2)`, id, hashes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// Exists reports whether withdrawal-service has a request with this id (or Settle key).
func (w *Withdrawals) Exists(ctx context.Context, id string) (bool, error) {
	id = strings.TrimPrefix(id, "withdrawal:")
	if !uuidRe.MatchString(id) {
		return false, nil
	}
	var n int
	err := w.Withdrawal.QueryRow(ctx, `SELECT count(*) FROM withdrawal_requests WHERE withdrawal_id = $1::uuid`, id).Scan(&n)
	return n > 0, err
}
