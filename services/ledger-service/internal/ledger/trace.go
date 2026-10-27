package ledger

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"digiwal/pkg/metrics"
)

// dbTracer feeds pkg/metrics' DB histograms from pgx's hooks: the time to get a connection (pgxpool.AcquireTracer), a statement as this service sees
// it (pgx.QueryTracer, until the last row is read), and how long a connection stays checked out (BeforeAcquire / AfterRelease).
type dbTracer struct {
	m    *metrics.DB
	held sync.Map // *pgx.Conn -> time it was handed out
}

type traceKey int

const (
	acquireStart traceKey = iota
	queryStart
)

func (t *dbTracer) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	return context.WithValue(ctx, acquireStart, time.Now())
}

func (t *dbTracer) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, d pgxpool.TraceAcquireEndData) {
	if s, ok := ctx.Value(acquireStart).(time.Time); ok && d.Err == nil {
		t.m.ObserveAcquireWait(time.Since(s))
	}
}

func (t *dbTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, queryStart, time.Now())
}

func (t *dbTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if s, ok := ctx.Value(queryStart).(time.Time); ok {
		t.m.ObserveQuery(time.Since(s))
	}
}

func (t *dbTracer) beforeAcquire(_ context.Context, c *pgx.Conn) bool {
	t.held.Store(c, time.Now())
	return true
}

func (t *dbTracer) afterRelease(c *pgx.Conn) bool {
	if s, ok := t.held.LoadAndDelete(c); ok {
		t.m.ObserveHold(time.Since(s.(time.Time)))
	}
	return true
}

// attach installs the hooks on cfg. A nil m installs nothing.
func attach(cfg *pgxpool.Config, m *metrics.DB) {
	if m == nil {
		return
	}
	t := &dbTracer{m: m}
	cfg.ConnConfig.Tracer = t
	cfg.BeforeAcquire = t.beforeAcquire
	cfg.AfterRelease = t.afterRelease
}
