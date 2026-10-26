# Load tests (k6 through api-gateway)

Run everything from **vm2** (ssh to vm3 and vm4 must work). k6 runs on vm4, next to the gateway. There is no Prometheus in the
loop: the Postgres containers are sampled from their cgroups, so the monitoring plane can stay stopped during a test.

| Script | What it does |
|---|---|
| `00-prepare.sh` | Pre-flight. Checks the services, waits until the Kafka consumers have caught up and the withdrawal pipeline is idle, writes `keys.json` on vm4 from the simulator's state, gives each user an active whitelisted address, and prints how many withdrawals the users can still afford. Prints `READY` or `NOT READY`. |
| `01-fund-users.sh` | Only if the users have too little ETH: sends `ETH_PER_USER` (default 5) to every deposit address on the mock chain. The ledger credits it after a few minutes (watch `00-prepare.sh`). |
| `10-step-test.sh` | Rising arrival rates (default 100..500 req/s, 60 s each, 20% withdrawals) -> one table row per step. Finds the knee. |
| `02-rate-limits.sh off\|on` | Switches api-gateway's rate limits (default 1200/min per IP, 600/min per key) off for a capacity test, and back on. |
| `30-diagnose.sh` | A short load (default 800 req/s, 40 s, balances) plus a bottleneck report: CPU of every container on every node, host user/system time, time per request at the gateway and the ledger, DB pool waits. No Prometheus needed. Run it when a result looks wrong. |
| `20-sustained.sh` | `RATE` req/s for `MINUTES` (default 500 for 60), one row per 5-minute chunk, so drift over time is visible. |

Every run refuses to start while the previous one is still being worked off (`FORCE=1` overrides). To keep k6 off the busiest node: `K6_HOST=vm3 GATEWAY_URL=http://10.10.0.3:8084 ./00-prepare.sh` (and the same variables for the run).

Typical order: `./00-prepare.sh`, (`./01-fund-users.sh`, `./00-prepare.sh`), `./10-step-test.sh`, `./20-sustained.sh`.
Each run writes `~/loadtest-results/<timestamp>-.../` with `environment.txt`, `table.txt`, `summary.json`, `k6.log` and the samples.

How to read the table: `achieved` is requests answered per second (k6 `http_reqs` of the step / duration); `dropped` are iterations k6 could
not start because earlier requests were still running (a sign the system is saturated); `PG ... cores/thr%/act` are CPU cores the
Postgres container used, the share of CFS periods in which it was throttled by its CPU cap, and the average active backends;
`backlog` is withdrawals accepted but not yet picked up by the signing pipeline at the end of the step.

Things to know before quoting a number
- The Postgres containers on vm2 (ledger) and vm4 (api_gateway) carry a 0.5-CPU cap on purpose (the scaling lab). `environment.txt` records it.
  To measure the hardware instead: `docker update --cpus 0 digiwal-dev-postgres-1` on vm2 and `...-postgres-gateway-1` on vm4 (and back to 0.5 afterwards).
- The chain is the mock chain, users are simulated, and the accepted withdrawals are really signed and "broadcast" on it, so the pipeline's backlog matters
  as much as the request latency: a rate the API accepts may be more than the pipeline can finish (`backlog`, and "pipeline drained in N s" at the end).
- Stop the simulator and the monitoring plane first (the exporters and the auditor's queries compete for the capped Postgres CPU).
  Bring monitoring back with `make monitoring` on vm2 and vm3 (and `make up` on vm4) from `dev/`.
- A run leaves its withdrawals in the databases; reset the environment (or accept the data) before a clean second series.
