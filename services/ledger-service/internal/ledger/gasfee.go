package ledger

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Gas Fee purposes (ledger.md invariant #12): why the gas was spent, stored for reporting.
var gasPurposes = map[string]bool{
	"withdrawal": true, "withdrawal_reverted": true, "nonce_filler": true,
	"gas_topup": true, "sweep": true, "treasury_sweep": true,
}

var txHashRe = regexp.MustCompile(`^0x[0-9a-fA-F]{64}$`)

// GasFee is a mined tx we signed, as its receipt reports it. The caller never sends a total or an
// account: the fee is gas_used × effective_gas_price, and both accounts are derived here.
type GasFee struct {
	ChainID           int64
	TxHash            string
	GasUsed           uint64
	EffectiveGasPrice *big.Int
	Purpose           string
}

// GasFeeResult is what POST /v1/ledger/gas-fees answers. TransactionID is empty when the fee is
// zero (nothing to post).
type GasFeeResult struct {
	TransactionID string
	Amount        *big.Int
}

// GasFeeKey is the idempotency key: one tx, one Gas Fee, whoever reports it.
func GasFeeKey(chainID int64, txHash string) string {
	return fmt.Sprintf("gas:%d:%s", chainID, strings.ToLower(txHash))
}

// PostGasFee records the gas of one mined tx exactly once (ledger.md invariant #12): in the chain's
// native asset, {Gas Expense Account −fee, Chain Account +fee}, in one transaction. A replay with the
// same values returns the existing transaction; different values for the same tx are
// ErrIdempotencyKeyReused. A user's Account is never touched.
func (s *Store) PostGasFee(ctx context.Context, g GasFee) (GasFeeResult, error) {
	bad := func(msg string) (GasFeeResult, error) { return GasFeeResult{}, fmt.Errorf("%w: %s", ErrInvalid, msg) }
	switch {
	case g.ChainID <= 0:
		return bad("chain_id must be positive")
	case !txHashRe.MatchString(g.TxHash):
		return bad("tx_hash must be a 0x-prefixed 32-byte hex hash")
	case g.GasUsed == 0:
		return bad("gas_used must be positive")
	case g.EffectiveGasPrice == nil || g.EffectiveGasPrice.Sign() < 0 || g.EffectiveGasPrice.Cmp(maxAmount) > 0:
		return bad("effective_gas_price must be a non-negative decimal string")
	case !gasPurposes[g.Purpose]:
		return bad("unknown purpose " + g.Purpose)
	}
	fee := new(big.Int).Mul(new(big.Int).SetUint64(g.GasUsed), g.EffectiveGasPrice)
	if fee.Cmp(maxAmount) > 0 {
		return bad("fee exceeds uint256")
	}
	if fee.Sign() == 0 { // a zero-price test chain: nothing was spent, nothing to post
		return GasFeeResult{Amount: fee}, nil
	}

	var asset string
	if err := s.pool.QueryRow(ctx, `SELECT id FROM assets WHERE asset_type = 'native' AND active`).Scan(&asset); errors.Is(err, pgx.ErrNoRows) {
		return GasFeeResult{}, fmt.Errorf("%w: no active native asset in the registry", ErrUnknownAsset)
	} else if err != nil {
		return GasFeeResult{}, fmt.Errorf("reading the native asset: %w", err) // more than one native asset also lands here
	}

	key := GasFeeKey(g.ChainID, g.TxHash)
	_, err := s.Apply(ctx, Transaction{
		IdempotencyKey: key, Source: SourceGasFee,
		Gas: &GasRef{ChainID: g.ChainID, TxHash: g.TxHash}, Purpose: g.Purpose,
		Entries: []Entry{
			{Ref: RefGasExpense, AssetID: asset, Amount: new(big.Int).Neg(fee)},
			{Ref: RefChain, AssetID: asset, Amount: fee},
		},
	})
	if err != nil {
		return GasFeeResult{}, err
	}
	var id string
	if err := s.pool.QueryRow(ctx, `SELECT id FROM ledger_transactions WHERE idempotency_key = $1`, key).Scan(&id); err != nil {
		return GasFeeResult{}, fmt.Errorf("reading the gas fee transaction: %w", err)
	}
	return GasFeeResult{TransactionID: id, Amount: fee}, nil
}
