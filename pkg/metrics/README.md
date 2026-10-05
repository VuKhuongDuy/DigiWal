# pkg/metrics

Shared Prometheus plumbing for every DigiWal service (`docs/design/monitoring-dashboard.md`, task 1). Same
pattern as `pkg/events`: a separate Go module, wired into a service with
`replace digiwal/pkg/metrics => ../../pkg/metrics` and copied in its Dockerfile.

```go
reg := metrics.NewRegistry()          // Go runtime + process collectors
reg.MustRegister(metrics.NewCached("ledger", 60*time.Second, 5*time.Second, func(ctx context.Context) ([]prometheus.Metric, error) {
    // query the service's OWN database; return samples built with metrics.Gauge / metrics.Counter
}))
go metrics.Serve(ctx, ":"+envOr("METRICS_PORT", metrics.DefaultPort), reg)  // GET /metrics, nothing else
```

- **A failed read is a gap.** If a `Read` errors or exceeds its deadline, the collector serves *no* series for that
  scrape and `digiwal_collector_up{collector}` is 0. It never serves a zero or the previous value as if current.
  Failures are retried on the next scrape, not cached.
- **Successful reads are cached** for the TTL (10s for on-chain figures, 60s for SQL aggregates) so a 15s scrape
  does not hit the database every time.
- **Counters are derived from the database at scrape time** (`metrics.Counter`): a monotonic figure computed from
  an append-only table, so history survives restarts, and Prometheus (`increase()`, `rate()`) does the bucketing.
- **Labels are an allow-list** (`metrics.AllowedLabels`): `asset`, `wallet`, `status`, `kind`, `rule`, `day`,
  `chain_id`, `service`, `job`, `result`, `flow`, `severity`, `component`, `collector`, `reason`, `state`, `caller`,
  `purpose`. `NewDesc` panics on anything else, so an identifier (user id, address, tx hash) can never become a label.
- **`Serve` serves `GET /metrics` only.** Never put it on a public listener (`api-gateway`'s especially); the port
  (`METRICS_PORT`, default 9100) is not published to the host.
