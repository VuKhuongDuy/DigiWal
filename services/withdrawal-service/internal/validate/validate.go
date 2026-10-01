// Package validate is a Withdrawal Request's validation step (docs/design/withdrawal-flow.md
// invariants #10, #12, #14, #18): everything that must pass before any ledger Reservation is
// attempted. It reads, never writes: the caller records the verdict.
package validate

import (
	"context"
	"math/big"
	"regexp"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"digiwal/services/withdrawal-service/internal/registry"
	"digiwal/services/withdrawal-service/internal/store"
)

// Reject reasons, returned to the client as reject_reason.
const (
	AssetNotRegistered    = "asset_not_registered"
	InvalidToAddress      = "invalid_to_address"
	AmountTooLarge        = "amount_too_large" // above uint256: can never be paid on-chain
	BelowMinimum          = "below_minimum_withdrawal_amount"
	NotWhitelisted        = "destination_not_whitelisted" // no entry, or still pending
	OnScreeningList       = "destination_on_screening_list"
	DestinationIsDeposit  = "destination_is_deposit_address" // via Destinations, task 3.10
	DestinationIsSystemWl = "destination_is_system_wallet"   // via Destinations, task 3.10
)

// Destinations refuses addresses that may never be a withdrawal destination: our own deposit
// addresses and the Hot/Warm Wallet (invariant #18, task 3.10). reason is one of the codes above,
// "" when allowed; err means the check could not be completed and the caller must fail closed.
type Destinations interface {
	Refuse(ctx context.Context, address common.Address) (reason string, err error)
}

type Whitelist interface {
	WhitelistStatus(ctx context.Context, userID, address string, cooldown time.Duration) (status string, ok bool, err error)
}

type Screening interface{ Contains(address string) bool }

type Validator struct {
	Assets    *registry.Registry
	Whitelist Whitelist
	Screening Screening
	Cooldown  time.Duration // Whitelist Cooldown
	// Destinations is nil until task 3.10; the check is skipped while it is.
	Destinations Destinations
}

var hexAddressRe = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

// Check runs every rule in the decided order (integration-contracts.md §2, POST /v1/withdrawals)
// and returns the first failing reason, or "" if all pass. Local checks come first; the only
// remote one (destinations) runs last, so it is reached only by requests that would otherwise
// pass. A non-nil err is transient: no verdict, and the request must not move on.
func (v *Validator) Check(ctx context.Context, w store.Withdrawal) (reason string, err error) {
	asset, ok := v.Assets.Get(w.AssetID)
	if !ok {
		return AssetNotRegistered, nil
	}
	if !hexAddressRe.MatchString(w.ToAddress) {
		return InvalidToAddress, nil
	}
	to := common.HexToAddress(w.ToAddress)
	if to == (common.Address{}) {
		return InvalidToAddress, nil
	}
	if w.Amount.Cmp(maxAmount) > 0 {
		return AmountTooLarge, nil
	}
	if w.Amount.Cmp(asset.MinWithdrawalAmount) < 0 {
		return BelowMinimum, nil
	}
	status, ok, err := v.Whitelist.WhitelistStatus(ctx, w.UserID, to.Hex(), v.Cooldown)
	if err != nil {
		return "", err
	}
	if !ok || status != store.WhitelistActive {
		return NotWhitelisted, nil
	}
	if v.Screening.Contains(to.Hex()) {
		return OnScreeningList, nil
	}
	if v.Destinations != nil {
		return v.Destinations.Refuse(ctx, to)
	}
	return "", nil
}

var maxAmount = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
