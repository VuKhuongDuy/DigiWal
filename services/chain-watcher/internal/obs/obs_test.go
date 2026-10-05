package obs_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"digiwal/pkg/metrics"
	"digiwal/services/chain-watcher/internal/obs"

	"github.com/prometheus/client_golang/prometheus"
)

type wm struct {
	n   int64
	ok  bool
	err error
}

func (w wm) Watermark(context.Context, int64) (int64, bool, error) { return w.n, w.ok, w.err }

type head struct {
	n   atomic.Int64
	err atomic.Bool
}

func (h *head) Head(context.Context) (int64, error) {
	if h.err.Load() {
		return 0, errors.New("rpc down")
	}
	return h.n.Load(), nil
}

func scrape(c prometheus.Collector) string {
	r := prometheus.NewRegistry()
	r.MustRegister(c)
	rec := httptest.NewRecorder()
	metrics.Handler(r).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

func TestLagFiguresAreExported(t *testing.T) {
	h := &obs.HeadTicker{}
	src := &head{}
	src.n.Store(1000)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, src, 10*time.Millisecond)
	c := obs.Collector(wm{n: 990, ok: true}, h, 31337, 2)
	waitFor(t, func() bool { return strings.Contains(scrape(c), "digiwal_chain_head_block") })
	out := scrape(c)
	for _, want := range []string{
		`digiwal_chainwatcher_watermark_block{chain_id="31337"} 990`, `digiwal_chain_head_block{chain_id="31337"} 1000`,
		`digiwal_chainwatcher_confirmation_depth{chain_id="31337"} 2`, `digiwal_chain_head_read_timestamp_seconds{chain_id="31337"}`,
		`digiwal_collector_up{collector="chain-watcher"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// The reason the head has its own ticker: when the poll loop is stuck the watermark stays and the head keeps moving,
// so the lag grows. Nothing here involves the poll loop at all.
func TestHeadKeepsAdvancingWhileTheWatermarkStandsStill(t *testing.T) {
	h := &obs.HeadTicker{}
	src := &head{}
	src.n.Store(100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, src, 5*time.Millisecond)
	c := obs.Collector(wm{n: 90, ok: true}, h, 1, 2) // the watermark never moves
	waitFor(t, func() bool { return strings.Contains(scrape(c), `chain_head_block{chain_id="1"} 100`) })
	src.n.Store(160)
	waitFor(t, func() bool { return strings.Contains(scrape(c), `chain_head_block{chain_id="1"} 160`) })
	if out := scrape(c); !strings.Contains(out, `watermark_block{chain_id="1"} 90`) {
		t.Fatalf("watermark moved:\n%s", out)
	}
}

// A failing RPC keeps the last head but stops advancing its read time, so staleness is visible; a head never read is absent.
func TestFailedHeadReadKeepsTheValueAndTheOldReadTime(t *testing.T) {
	h := &obs.HeadTicker{}
	src := &head{}
	src.err.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, src, 5*time.Millisecond)
	c := obs.Collector(wm{n: 1, ok: true}, h, 1, 2)
	time.Sleep(60 * time.Millisecond)
	if out := scrape(c); strings.Contains(out, "digiwal_chain_head_block") || strings.Contains(out, "head_read_timestamp") {
		t.Fatalf("never read: no series, not 0:\n%s", out)
	}
	src.err.Store(false)
	src.n.Store(50)
	waitFor(t, func() bool { return strings.Contains(scrape(c), `chain_head_block{chain_id="1"} 50`) })
	first := scrape(c)
	src.err.Store(true)
	src.n.Store(999)
	time.Sleep(60 * time.Millisecond)
	out := scrape(c)
	if !strings.Contains(out, `chain_head_block{chain_id="1"} 50`) || strings.Contains(out, "} 999") {
		t.Fatalf("a failed read must not change the head:\n%s", out)
	}
	if line(first, "head_read_timestamp") != line(out, "head_read_timestamp") {
		t.Fatalf("read time advanced on failed reads:\n%s\n%s", line(first, "head_read_timestamp"), line(out, "head_read_timestamp"))
	}
}

func line(s, sub string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) && !strings.HasPrefix(l, "#") {
			return l
		}
	}
	return ""
}

func TestWatermarkAbsentBeforeFirstScanAndGapOnDBError(t *testing.T) {
	h := &obs.HeadTicker{}
	if out := scrape(obs.Collector(wm{ok: false}, h, 1, 2)); strings.Contains(out, "watermark_block") || !strings.Contains(out, "confirmation_depth") {
		t.Fatalf("\n%s", out)
	}
	out := scrape(obs.Collector(wm{err: errors.New("db down")}, h, 1, 2))
	if strings.Contains(out, "watermark_block") || strings.Contains(out, "confirmation_depth") || !strings.Contains(out, `collector="chain-watcher"} 0`) {
		t.Fatalf("\n%s", out)
	}
}
