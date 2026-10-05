package obs_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"digiwal/pkg/metrics"
	"digiwal/services/wallet-service/internal/obs"
	"digiwal/services/wallet-service/internal/storage"

	"github.com/prometheus/client_golang/prometheus"
)

type fake struct {
	st  []storage.NonceState
	err error
}

func (f fake) NonceStates(context.Context) ([]storage.NonceState, error) { return f.st, f.err }

func scrape(c prometheus.Collector) string {
	r := prometheus.NewRegistry()
	r.MustRegister(c)
	rec := httptest.NewRecorder()
	metrics.Handler(r).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

func u(n uint64) *uint64 { return &n }

func TestFrozenAndHealthyChains(t *testing.T) {
	out := scrape(obs.Collector(fake{st: []storage.NonceState{{ChainID: 1, NextNonce: 9}, {ChainID: 31337, NextNonce: 12, FrozenFrom: u(7)}}}))
	for _, want := range []string{
		`digiwal_hot_wallet_frozen{chain_id="1"} 0`, `digiwal_hot_wallet_frozen{chain_id="31337"} 1`,
		`digiwal_hot_wallet_frozen_from_nonce{chain_id="31337"} 7`, `digiwal_hot_wallet_next_nonce{chain_id="31337"} 12`,
		`digiwal_collector_up{collector="wallet-service"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, `frozen_from_nonce{chain_id="1"}`) {
		t.Errorf("a chain that is not frozen has no frozen_from series:\n%s", out)
	}
}

// A database that cannot be read must never read as "not frozen".
func TestUnreadableDatabaseIsNotReportedAsNotFrozen(t *testing.T) {
	out := scrape(obs.Collector(fake{err: errors.New("db down")}))
	if strings.Contains(out, "digiwal_hot_wallet_frozen") || !strings.Contains(out, `collector="wallet-service"} 0`) {
		t.Fatalf("\n%s", out)
	}
}

func TestNeverSignedMeansNoSeries(t *testing.T) {
	if out := scrape(obs.Collector(fake{})); strings.Contains(out, "digiwal_hot_wallet") || !strings.Contains(out, `collector="wallet-service"} 1`) {
		t.Fatalf("\n%s", out)
	}
}
