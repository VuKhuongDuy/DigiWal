# dev — the always-on development environment

A long-lived stack with a **simulator** that keeps a population of users depositing and withdrawing, so there is
always realistic data and traffic. Unlike `e2e/` (throwaway, deterministic, for tests) it restarts by itself and
keeps its data. It runs on **two nodes** (vm2 and vm3, joined by a WireGuard network); the design and the reasoning are
in [`docs/design/multi-node-deployment.md`](../docs/design/multi-node-deployment.md).

## Who runs where

| | vm2 (`10.10.0.1`) — request path | vm3 (`10.10.0.2`) — custody and chain |
|---|---|---|
| Postgres (own server per node, no replica) | `ledger`, `api_gateway` | `custody` (wallet-service), `withdrawal`, `chain_watcher` |
| Other stores | mongo | kafka, redis, anvil |
| Services | api-gateway, ledger-service, history-service | wallet-service, withdrawal-service, chain-watcher, sweeper, simulator |
| Auditor and monitoring | node, cAdvisor and Postgres exporters | the auditor (and its `auditor` database), Prometheus, Grafana, Kafka exporter, node, cAdvisor and Postgres exporters |

Each service sits on the same node as its own database. Cross-node calls keep the **service name** (`extra_hosts` points
it at the peer's private address), so the mTLS certificates need no change. The exact placement, ports and settings are
in [`nodes/vm2.yml`](nodes/vm2.yml) and [`nodes/vm3.yml`](nodes/vm3.yml).

## Using it

`make` reads which node it is on from `/etc/digiwal/node` (`vm2` or `vm3`); without that file everything runs on the
one host, as before.

```bash
make up        # start / update THIS node's part (safe to re-run; the first run creates .env and the wallet seeds)
make status    # container state and ports
make verify    # WireGuard, the peer's public side exposes only ssh, private ports reachable, no public bindings
make sim-logs  # (vm3) what the simulated users are doing
make users     # (vm3) simulated users, deposit addresses, live balances
make logs SVC=ledger-service
make down      # stop, KEEP data   |   make reset   # stop and DROP this node's data
make push-certs  # (vm2) copy vm3's leaf certificates; the CA key never leaves vm2
make firewall    # install and enable the host firewall (infra/node/)
```

Starting both nodes from nothing: run `make up` on vm3 (its stores come up first), then on vm2 (its ledger-service needs
vm3's Kafka), then on vm3 again (its withdrawal-service and sweeper need vm2's ledger-service). Each run is idempotent.
Whether every service retries by itself when started out of order is not verified (the design note lists it); this order
is the one used for the cutover. Secrets (database passwords, Mongo, and on vm3 the master key) are generated once per node into
the gitignored `.env`; **the master key exists only on vm3** and the sealed seeds in Postgres are useless without it.

## Auditor and monitoring

The auditor ([`services/auditor`](../services/auditor/README.md), dev only) runs on vm3 and reads the other services' stores
with a read-only login: `auditor_ro` on vm2's `ledger` (over the private network) and on vm3's `custody`, `withdrawal` and
`chain_watcher`, and on the `history` Mongo database on vm2. `make up` creates those logins (`make audit-roles`, on each node,
vm2 first). It keeps its own findings in the `auditor` database on vm3.

Prometheus on vm3 (`infra/prometheus/prometheus.multi-node.yml`) scrapes every service's `/metrics` (vm2's are published on
`10.10.0.1:9101` api-gateway and `:9102` ledger-service), the node, cAdvisor and Postgres exporters of both nodes, and the
Kafka exporter. Rules: `infra/prometheus/alerts.yml` and `alerts-node.yml` (memory, CPU/IO pressure, disk, OOM kills,
Postgres connections, Kafka lag), tested by `infra/prometheus/check.sh`. Grafana on vm3 has the Business and Engineering
dashboards. Both are bound to `127.0.0.1` on vm3, so reach them through a tunnel:

```bash
ssh -L 3000:127.0.0.1:3000 -L 9090:127.0.0.1:9090 vm3     # Grafana http://localhost:3000, Prometheus http://localhost:9090
```

**Alerts to Telegram.** Prometheus sends firing alerts to Alertmanager (vm3, `127.0.0.1:9093`), which posts them to a
Telegram chat: one message per group of alerts that fire together (30 s), a reminder every 4 h while still firing, and a
"resolved" message. Create a bot with @BotFather, add it to the chat, and put its token and the chat id (a negative number
for a group) in vm3's `dev/.env`, then `make monitoring` there:

```
TELEGRAM_BOT_TOKEN=123456:ABC...
TELEGRAM_CHAT_ID=-1001234567890
```

Without them alerts are held and visible on the Alertmanager page only. (Grafana's own Alertmanager does not accept alerts
from Prometheus in the version we run, so Grafana is not in this path.)

Set `RECON_CAPITAL` (and `RECON_TOLERANCE`) in vm3's `.env` to what the operator put into custody that no user owns
(the Hot Wallet's liquidity), for example `RECON_CAPITAL=ETH=<minor units>,TT=<minor units>`; until then the
reconciliation reports that capital as a residual (C1).

## Access and security

Nothing publishes on a public address: every port binds to `127.0.0.1` or the node's `10.10.0.x`, and the host
firewall (INPUT and `DOCKER-USER`, `infra/node/firewall.sh`) drops anything else arriving on the public interface
except SSH. Reach databases, the gateway and the like with SSH tunnels, for example:

```bash
ssh -L 5432:127.0.0.1:5432 vm2      # ledger / api_gateway (the role and password are in vm2's dev/.env)
ssh -L 8084:127.0.0.1:8084 vm2      # api-gateway
```

Each service connects with its own Postgres role (`svc_*`, scram, only to its own database); the superuser `custody`
is reachable only from inside the container. Mongo has a `svc_history` user.

## What is different from e2e

| | e2e (`make e2e`) | dev (`make up`) |
|---|---|---|
| lifetime | torn down with its volumes after the tests | `restart: unless-stopped`, named volumes survive `down` and reboots |
| Anvil | automine, fresh chain, in memory | a block every 2s, chain persisted (`--state`, dumped every 90s), `--prune-history 128`, 2.5 GB cap |
| master key / DB credentials | fixed throwaway | random, generated once per node into `.env` |
| Reaper timeouts | 20s | 5 min (the decided values) |

Confirmation Depth stays at 2 and the Whitelist Cooldown at 2s, so a withdrawal is confirmed in about 6-10s.

## The simulator (`sim run`, `e2e/cmd/sim`)

Users sign up gradually up to `SIM_USERS` (default 25; whales, regulars and lurkers). Actions arrive at random, on
average `SIM_RATE` per minute (default 12; 60 is the load used so far), `SIM_WORKERS` in flight: deposits of ETH/TT (on
chain, from Anvil's funded accounts), withdrawals to known and new whitelisted addresses, balance checks, retries of a
pending withdrawal with the same idempotency key, and the occasional overdraw that must be rejected. Set them in `.env`
on vm3 and `make up`.

It is also a checker. Per user it keeps *deposits sent − withdrawals confirmed* and compares it with the ledger
balance once the user has been quiet for 90s. Look for these in `make sim-logs`: `DRIFT` (the ledger disagrees with what
really moved), `ANOMALY` (an overdraw was accepted, a retry was not recognised, a balance is inconsistent), and
`withdrawal ended badly`. A `stats` line every minute summarises the run (`errors=0 drift=0` is healthy).

## Caveats

- **Anvil is the weak point.** It dumps its whole state to one file in place; a hard kill (OOM, power loss) loses the
  blocks since the last dump and can truncate the file. The services then see the chain behind them: chain-watcher logs a
  reorg, and the Hot Wallet's next nonce is ahead of the chain, so withdrawals queue behind a Nonce Gap. Recovery: the
  operator steps in `services/withdrawal-service/README.md` (rebroadcast the signed transactions from
  `hot_wallet_signatures` in nonce order), or `make reset` on both nodes. This happened once and was recovered that way.
- Testnet only: Anvil's public dev keys, plain-HTTP gateway (private network only), no rate limiting.
