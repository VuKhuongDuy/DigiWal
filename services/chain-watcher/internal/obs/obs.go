// Package obs is chain-watcher's metrics (monitoring-dashboard.md section 6). The point of the lag figures is that
// they must not depend on the poll loop they are supposed to watch.
package obs

import (
	"context"
	"strconv"
	"sync/atomic"
	"time"

	"digiwal/pkg/metrics"

	"github.com/prometheus/client_golang/prometheus"
)

// Watermark is the last block fully scanned (store.Store).
type Watermark interface {
	Watermark(ctx context.Context, chainID int64) (n int64, ok bool, err error)
}

// Head is the chain head (chain.Eth).
type Head interface {
	Head(ctx context.Context) (int64, error)
}

var (
	watermarkDesc = metrics.NewDesc("digiwal_chainwatcher_watermark_block", "Last block chain-watcher has fully scanned. Absent before its first scan.", "chain_id")
	headDesc      = metrics.NewDesc("digiwal_chain_head_block", "Latest block of the RPC node chain-watcher uses, read by its own ticker (not the poll loop).", "chain_id")
	headReadDesc  = metrics.NewDesc("digiwal_chain_head_read_timestamp_seconds", "When that head was last read successfully (Unix seconds); time() minus this is the RPC read health.", "chain_id")
	depthDesc     = metrics.NewDesc("digiwal_chainwatcher_confirmation_depth", "Blocks a deposit waits behind the watermark before it is confirmed: the expected lag floor.", "chain_id")
)

// HeadTicker reads the chain head every interval on its own goroutine. If the poll loop hangs, the watermark freezes
// while this keeps advancing, so the lag grows visibly instead of both numbers standing still and reading as zero. A
// failed read keeps the last head but does not advance its read time, so staleness shows.
type HeadTicker struct {
	head, readAt atomic.Int64
}

// Run reads the head now and then every interval until ctx ends.
func (h *HeadTicker) Run(ctx context.Context, src Head, every time.Duration) {
	read := func() {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if n, err := src.Head(rctx); err == nil {
			h.head.Store(n)
			h.readAt.Store(time.Now().Unix())
		}
	}
	read()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			read()
		}
	}
}

// Collector exports watermark, head and depth for one chain. The head series are absent until the first successful
// read (no fake zero).
func Collector(w Watermark, h *HeadTicker, chainID, depth int64) prometheus.Collector {
	id := strconv.FormatInt(chainID, 10)
	db := metrics.NewCached("chain-watcher", 15*time.Second, 5*time.Second, func(ctx context.Context) ([]prometheus.Metric, error) {
		n, ok, err := w.Watermark(ctx, chainID)
		if err != nil {
			return nil, err
		}
		out := []prometheus.Metric{metrics.Gauge(depthDesc, float64(depth), id)}
		if ok {
			out = append(out, metrics.Gauge(watermarkDesc, float64(n), id))
		}
		return out, nil
	})
	return &combined{db: db, h: h, id: id}
}

type combined struct {
	db *metrics.Cached
	h  *HeadTicker
	id string
}

func (c *combined) Describe(chan<- *prometheus.Desc) {}
func (c *combined) Collect(out chan<- prometheus.Metric) {
	c.db.Collect(out)
	if at := c.h.readAt.Load(); at != 0 {
		out <- metrics.Gauge(headDesc, float64(c.h.head.Load()), c.id)
		out <- metrics.Gauge(headReadDesc, float64(at), c.id)
	}
}
