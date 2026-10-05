package obs

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"digiwal/pkg/metrics"
	"digiwal/services/withdrawal-service/internal/registry"

	"github.com/ethereum/go-ethereum/common"
	"github.com/prometheus/client_golang/prometheus"
)

// Balances reads on-chain balances (chain.Eth).
type Balances interface {
	Balance(ctx context.Context, addr common.Address) (*big.Int, error)
	TokenBalance(ctx context.Context, token, addr common.Address) (*big.Int, error)
}

// Wallets are the treasury tiers' public addresses; Cold is the zero address when none is declared.
type Wallets struct{ Hot, Warm, Cold common.Address }

var (
	walletBalance = metrics.NewDesc("digiwal_wallet_balance", "On-chain balance of a treasury wallet, whole asset units. On-chain figure, read at scrape time.", "wallet", "asset")
	walletRead    = metrics.NewDesc("digiwal_wallet_balance_read_timestamp_seconds", "When that balance was read (Unix seconds), so staleness is visible.", "wallet", "asset")
)

// WalletCollector reads the Hot, Warm and (if declared) Cold balance of every registered asset from the chain.
// The balances are not taken from the ledger: it tracks user liabilities, not treasury tiers (wallet-tiering.md
// invariant #5). A balance that cannot be read is a missing series, not a zero: the others are still served,
// and collector_up reports the failure (design invariant 4).
func WalletCollector(b Balances, assets *registry.Registry, w Wallets) prometheus.Collector {
	tiers := []struct {
		name string
		addr common.Address
	}{{"hot", w.Hot}, {"warm", w.Warm}, {"cold", w.Cold}}
	return metrics.NewCached("wallets", 10*time.Second, 5*time.Second, func(ctx context.Context) ([]prometheus.Metric, error) {
		var out []prometheus.Metric
		var errs []error
		for _, a := range assets.All() {
			for _, t := range tiers {
				if t.addr == (common.Address{}) {
					continue
				}
				var v *big.Int
				var err error
				if a.Type == "native" {
					v, err = b.Balance(ctx, t.addr)
				} else {
					v, err = b.TokenBalance(ctx, common.HexToAddress(a.ContractAddress), t.addr)
				}
				if err != nil {
					errs = append(errs, fmt.Errorf("%s %s: %w", t.name, a.Symbol, err))
					continue
				}
				out = append(out,
					metrics.Gauge(walletBalance, Whole(v, a.Decimals), t.name, a.Symbol),
					metrics.Gauge(walletRead, float64(time.Now().Unix()), t.name, a.Symbol))
			}
		}
		if len(errs) > 0 {
			return out, metrics.Partial(errors.Join(errs...))
		}
		return out, nil
	})
}
