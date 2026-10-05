// Package obs is wallet-service's metrics. Only operational numbers: nonce bookkeeping and whether signing is frozen.
// Nothing key-related, no address, no idempotency key (monitoring-dashboard.md invariant 3).
package obs

import (
	"context"
	"strconv"
	"time"

	"digiwal/pkg/metrics"
	"digiwal/services/wallet-service/internal/storage"

	"github.com/prometheus/client_golang/prometheus"
)

// Source is the store.
type Source interface {
	NonceStates(ctx context.Context) ([]storage.NonceState, error)
}

var (
	frozen     = metrics.NewDesc("digiwal_hot_wallet_frozen", "1 while Hot Wallet signing is frozen by a Nonce Gap on the chain, else 0. wallet-service owns the freeze, so this is the authoritative signal.", "chain_id")
	frozenFrom = metrics.NewDesc("digiwal_hot_wallet_frozen_from_nonce", "The lowest nonce of the open Nonce Gap; absent when not frozen.", "chain_id")
	nextNonce  = metrics.NewDesc("digiwal_hot_wallet_next_nonce", "Next Hot Wallet nonce wallet-service will hand out (the auditor compares the chain's transaction count with it).", "chain_id")
)

// Collector exports the Hot Wallet's nonce state per chain. A chain that has never signed has no row, so no series.
func Collector(s Source) prometheus.Collector {
	return metrics.NewCached("wallet-service", 10*time.Second, 5*time.Second, func(ctx context.Context) ([]prometheus.Metric, error) {
		states, err := s.NonceStates(ctx)
		if err != nil {
			return nil, err
		}
		var out []prometheus.Metric
		for _, st := range states {
			id := strconv.FormatInt(st.ChainID, 10)
			f := 0.0
			if st.FrozenFrom != nil {
				f = 1
				out = append(out, metrics.Gauge(frozenFrom, float64(*st.FrozenFrom), id))
			}
			out = append(out, metrics.Gauge(frozen, f, id), metrics.Gauge(nextNonce, float64(st.NextNonce), id))
		}
		return out, nil
	})
}
