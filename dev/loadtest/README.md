# Load tests (k6 through api-gateway)

Run everything from **vm2** (ssh to vm3 and vm4 must work). k6 runs on vm2; api-gateway is on vm3 and ledger-service on vm4, so the load generator, the gateway and the ledger are on three different nodes. There is no Prometheus in the
loop: the Postgres containers are sampled from their cgroups, so the monitoring plane can stay stopped during a test.

| Script | What it does |
|---|---|
| `00-prepare.sh` | Pre-flight. Checks the services, waits until the Kafka consumers have caught up and the withdrawal pipeline is idle, writes `keys.json` on vm4 from the simulator's state, gives each user an active whitelisted address, and prints how many withdrawals the users can still afford. Prints `READY` or `NOT READY`. |
| `01-fund-users.sh` | Only if the users have too little ETH: sends `ETH_PER_USER` (default 5) to every deposit address on the mock chain. The ledger credits it after a few minutes (watch `00-prepare.sh`). |
| `10-step-test.sh` | Rising arrival rates (default 100..500 req/s, 60 s each, 20% withdrawals) -> one table row per step. Finds the knee. |
| `02-rate-limits.sh off\|on` | Switches api-gateway's rate limits (default 1200/min per IP, 600/min per key) off for a capacity test, and back on. |
| `30-diagnose.sh` | A short load (default 800 req/s, 40 s, balances) plus a bottleneck report: CPU of every container on every node, host user/system time, time per request at the gateway and the ledger, DB pool waits. No Prometheus needed. Run it when a result looks wrong. |
| `20-sustained.sh` | `RATE` req/s for `MINUTES` (default 500 for 60), one row per 5-minute chunk, so drift over time is visible. |

Every run refuses to start while the previous one is still being worked off (`FORCE=1` overrides). `K6_HOST`, `GATEWAY_URL` and `GATEWAY_NODE` (defaults vm2, http://10.10.0.2:8084, vm3) say where k6 runs and where the gateway is.

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

## Running k6 from a laptop (macOS)

The gateways listen only on `127.0.0.1:8084` of vm3 and vm4 (and on the WireGuard address), never on a public address, so a laptop reaches them
through SSH tunnels. Nothing needs to be set up on the servers (sshd allows TCP forwarding).

1. Raise the file-descriptor limit in the shell that starts the tunnels AND k6 (macOS defaults to 256): `ulimit -n 65536`.
2. Several tunnels per gateway, not one: every connection of a tunnel shares one TCP connection, so one lost packet stalls all of them.
   ```bash
   for p in 18084 18085 18086 18087; do ssh -f -N -o ServerAliveInterval=30 -o ExitOnForwardFailure=yes -L $p:127.0.0.1:8084 alex_vm_3; done
   for p in 18094 18095 18096 18097; do ssh -f -N -o ServerAliveInterval=30 -o ExitOnForwardFailure=yes -L $p:127.0.0.1:8084 alex_vm_4; done
   ```
3. Copy the script and the API keys (the keys of the simulated users; they are written by `00-prepare.sh` on vm2):
   ```bash
   mkdir -p ~/k6 && scp alex_vm_2:/root/wt-loadtest/dev/loadtest/k6/mix.js ~/k6/ && scp alex_vm_2:/tmp/loadtest/keys.json ~/k6/
   ```
4. Run (`brew install k6` once). BASES lists the local tunnel ports, so every request picks one at random:
   ```bash
   B=http://127.0.0.1; k6 run -e KEYS=$HOME/k6/keys.json -e MIX=balances -e STEPS=2000,2500,3000 -e STEP_S=60 -e GAP_S=15 \
     -e BASES=$B:18084,$B:18085,$B:18086,$B:18087,$B:18094,$B:18095,$B:18096,$B:18097 \
     --summary-export=$HOME/k6/summary.json ~/k6/mix.js 2>&1 | tee $HOME/k6/k6.log
   ```
5. While it runs, on vm2: `./40-watch.sh 1200` records the servers' side (CPU of the three nodes, pool waits, cache hit ratios, gateway request rate).
6. To get the per-step table from the laptop's run, copy `summary.json` and `k6.log` into a directory on vm2 and run
   `python3 lib/analyze.py <dir> 2000,2500,3000 60 15` (the Postgres columns say n/a: they come from the samplers `10-step-test.sh` starts).

What the laptop run measures: the system plus an internet path and an SSH tunnel. Latencies include the round trip from the laptop to the VMs, and sshd on vm3/vm4 spends some CPU
on the tunnel, so compare numbers only with other laptop runs. Run `./00-prepare.sh` on vm2 first so the keys are fresh and the system is idle.

### Laptop shortcut
`./mac-k6.sh` does steps 1–4 above in one go (opens the eight tunnels with `./tunnels.sh up`, copies `keys.json` into `k6/` when it is missing, runs k6 and writes `k6/results/<time>/`).
`./tunnels.sh status` shows which tunnels work; `./tunnels.sh down` closes them.
