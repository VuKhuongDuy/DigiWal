package obs_test

import (
	"context"
	"errors"
	"math/big"
	"net/http/httptest"
	"strings"
	"testing"

	"digiwal/pkg/metrics"
	"digiwal/services/ledger-service/internal/ledger"
	"digiwal/services/ledger-service/internal/obs"

	"github.com/prometheus/client_golang/prometheus"
)

type fake struct {
	st  ledger.Stats
	err error
}

func (f fake) Stats(context.Context) (ledger.Stats, error) { return f.st, f.err }

func scrape(c prometheus.Collector) string {
	r := prometheus.NewRegistry()
	r.MustRegister(c)
	rec := httptest.NewRecorder()
	metrics.Handler(r).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

func TestExportsWholeUnitsAndTypes(t *testing.T) {
	oneEth := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	out := scrape(obs.Collector(fake{st: ledger.Stats{
		Assets:         []ledger.AssetStats{{ID: "a", Symbol: "ETH", Decimals: 18, DepositsCredited: 3, DepositAmount: new(big.Int).Mul(oneEth, big.NewInt(5)), UserLiabilities: new(big.Int).Div(oneEth, big.NewInt(2))}},
		ActiveUserDays: 9, FailedEvents: 1, ActiveLocks: 2, Flagged: map[string]int64{"pending": 1, "approved": 0, "rejected": 0},
	}}))
	for _, want := range []string{
		"# TYPE digiwal_deposits_credited_total counter", `digiwal_deposits_credited_total{asset="ETH"} 3`,
		"# TYPE digiwal_deposit_amount_total counter", `digiwal_deposit_amount_total{asset="ETH"} 5`,
		"# TYPE digiwal_ledger_active_user_days_total counter", "digiwal_ledger_active_user_days_total 9",
		`digiwal_ledger_user_liabilities{asset="ETH"} 0.5`, "digiwal_ledger_failed_events 1", "digiwal_ledger_active_locks 2",
		`digiwal_ledger_flagged_deposits{status="pending"} 1`, `digiwal_ledger_flagged_deposits{status="approved"} 0`,
		`digiwal_collector_up{collector="ledger-service"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestUnreadableLedgerIsAGap(t *testing.T) {
	out := scrape(obs.Collector(fake{err: errors.New("db down")}))
	if strings.Contains(out, "digiwal_deposits_credited_total") || !strings.Contains(out, `digiwal_collector_up{collector="ledger-service"} 0`) {
		t.Fatalf("\n%s", out)
	}
}
