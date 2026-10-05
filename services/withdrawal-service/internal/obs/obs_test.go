package obs_test

import (
	"context"
	"errors"
	"math/big"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"digiwal/pkg/metrics"
	"digiwal/services/withdrawal-service/internal/obs"
	"digiwal/services/withdrawal-service/internal/registry"
	"digiwal/services/withdrawal-service/internal/store"

	"github.com/prometheus/client_golang/prometheus"
)

type fake struct {
	st       store.Stats
	err      error
	stuckBc  int64
	headSeen int64
	gotBlk   int64
}

func (f *fake) Stats(context.Context, store.StuckThresholds) (store.Stats, error) { return f.st, f.err }
func (f *fake) StuckBroadcast(_ context.Context, head, blocks int64) (int64, error) {
	f.headSeen, f.gotBlk = head, blocks
	return f.stuckBc, nil
}

type head struct {
	n   int64
	err error
}

func (h head) Head(context.Context) (int64, error) { return h.n, h.err }

func scrape(c prometheus.Collector) string {
	r := prometheus.NewRegistry()
	r.MustRegister(c)
	rec := httptest.NewRecorder()
	metrics.Handler(r).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

var th = store.StuckThresholds{Request: 300e9, Reservation: 300e9, BroadcastBlocks: 128}

func assets() *registry.Registry {
	return registry.New(registry.Asset{ID: "id-eth", Symbol: "ETH", Decimals: 18})
}

func sample() store.Stats {
	oneEth := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	st := store.Stats{
		Created: 10, ByStatus: map[string]int64{}, Confirmed: map[string]*big.Int{"id-eth": new(big.Int).Mul(oneEth, big.NewInt(3)), "id-gone": big.NewInt(5)},
		Failures: []store.Failure{{Status: "failed", Reason: "tx_reverted", Count: 2}, {Status: "failed", Reason: "something we never wrote", Count: 1},
			{Status: "rejected", Reason: "destination_not_whitelisted", Count: 4}, {Status: "failed", Reason: "", Count: 1}},
		TimedCount: 2, ProcessingSecs: 30, ChainWaitSecs: 400,
		Stuck: map[string]int64{"requested": 0, "reserved": 1}, OldestAge: map[string]float64{"reserved": 77},
		NonceGaps: 2, NonceGapsUnreported: 1, WhitelistBlocked: 3, OutboxUnpublished: 4, OutboxOldestAge: 42,
	}
	for _, s := range store.AllStatuses {
		st.ByStatus[s] = 0
	}
	st.ByStatus["confirmed"], st.ByStatus["failed"], st.ByStatus["rejected"], st.ByStatus["disputed"] = 3, 4, 4, 1
	return st
}

func TestExports(t *testing.T) {
	out := scrape(obs.Collector(&fake{st: sample()}, assets(), th))
	for _, want := range []string{
		"# TYPE digiwal_withdrawals_total counter",
		`digiwal_withdrawals_total{status="requested"} 10`, `digiwal_withdrawals_total{status="confirmed"} 3`,
		`digiwal_withdrawals_total{status="failed"} 4`, `digiwal_withdrawals_total{status="rejected"} 4`,
		`digiwal_withdrawal_amount_total{asset="ETH"} 3`, `digiwal_withdrawal_amount_total{asset="unknown"}`,
		`digiwal_withdrawal_failures_total{reason="tx_reverted",status="failed"} 2`,
		`digiwal_withdrawal_failures_total{reason="destination_not_whitelisted",status="rejected"} 4`,
		"digiwal_withdrawal_processing_seconds_sum 30", "digiwal_withdrawal_chain_wait_seconds_sum 400", "digiwal_withdrawal_timed_total 2",
		`digiwal_withdrawal_requests{status="disputed"} 1`, `digiwal_withdrawal_requests{status="signed"} 0`,
		`digiwal_withdrawal_stuck{status="reserved"} 1`, `digiwal_withdrawal_oldest_age_seconds{status="reserved"} 77`,
		`digiwal_nonce_gaps{state="unreported"} 1`, `digiwal_nonce_gaps{state="total"} 2`, "digiwal_whitelist_blocked 3",
		"digiwal_outbox_unpublished 4", "digiwal_outbox_oldest_unpublished_age_seconds 42",
		`digiwal_collector_up{collector="withdrawal-service"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// Design invariant 3: the reason label is a closed set. Free text in a column can never become a label value.
func TestReasonLabelIsBounded(t *testing.T) {
	out := scrape(obs.Collector(&fake{st: sample()}, assets(), th))
	if strings.Contains(out, "something we never wrote") {
		t.Fatalf("an unknown reason leaked into a label:\n%s", out)
	}
	for _, want := range []string{`reason="other",status="failed"} 1`, `reason="none",status="failed"} 1`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestUnreadableDatabaseIsAGap(t *testing.T) {
	out := scrape(obs.Collector(&fake{err: errors.New("db down")}, assets(), th))
	if strings.Contains(out, "digiwal_withdrawals_total") || !strings.Contains(out, `digiwal_collector_up{collector="withdrawal-service"} 0`) {
		t.Fatalf("\n%s", out)
	}
}

func TestBroadcastStuckUsesTheChainHeadAndFailsOnItsOwn(t *testing.T) {
	f := &fake{stuckBc: 2}
	out := scrape(obs.BroadcastCollector(f, head{n: 500}, 128))
	if !strings.Contains(out, `digiwal_withdrawal_stuck{status="broadcast"} 2`) || f.headSeen != 500 || f.gotBlk != 128 {
		t.Fatalf("head %d blocks %d:\n%s", f.headSeen, f.gotBlk, out)
	}
	out = scrape(obs.BroadcastCollector(f, head{err: errors.New("rpc down")}, 128))
	if strings.Contains(out, "digiwal_withdrawal_stuck") || !strings.Contains(out, `collector="withdrawal-service-broadcast"} 0`) {
		t.Fatalf("an RPC failure is a gap in this one series:\n%s", out)
	}
	// the database collector is unaffected by it
	if out := scrape(obs.Collector(&fake{st: sample()}, assets(), th)); !strings.Contains(out, `collector="withdrawal-service"} 1`) {
		t.Fatal("independent collectors")
	}
}

func TestStageHistogram(t *testing.T) {
	reg := prometheus.NewRegistry()
	observe := obs.Stages(reg)
	observe("signed", "broadcast", 2*time.Second)
	observe("broadcast", "confirmed", 200*time.Second)
	observe("broadcast", "confirmed", 400*time.Second)
	rec := httptest.NewRecorder()
	metrics.Handler(reg).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	out := rec.Body.String()
	for _, want := range []string{
		`digiwal_withdrawal_stage_duration_seconds_count{stage="signed_to_broadcast"} 1`,
		`digiwal_withdrawal_stage_duration_seconds_count{stage="broadcast_to_confirmed"} 2`,
		`digiwal_withdrawal_stage_duration_seconds_sum{stage="broadcast_to_confirmed"} 600`,
		`digiwal_withdrawal_stage_duration_seconds_bucket{stage="broadcast_to_confirmed",le="300"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
