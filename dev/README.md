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

Each service sits on the same node as its own database. Cross-node calls keep the **service name** (`extra_hosts` points
it at the peer's private address), so the mTLS certificates need no change. The exact placement, ports and settings are
in [`nodes/vm2.yml`](nodes/vm2.yml) and [`nodes/vm3.yml`](nodes/vm3.yml); not yet deployed: Prometheus, Grafana, the
auditor (see the design note).

## Using it

`make` reads which node it is on from `/etc/digiwal/node` (`vm2` or `vm3`); without that file everything runs on the
one host, as before.

```bash
make up        # start / update THIS node's part (safe to re-run; the first run creates .env and the wallet seeds)
make status    # container state and ports
make verify    # PASS/FAIL per check: WireGuard, public side only ssh, no public bindings, hardening flags of hsm-gateway/wallet-service/sweep-signer, key-file modes and owners, no key/PIN in any env, no CA key on the node, vault grants, audit chain, swap/ptrace (SKIP = needs the node itself)
make sim-logs  # (vm3) what the simulated users are doing
make users     # (vm3) simulated users, deposit addresses, live balances
make logs SVC=ledger-service
make down      # stop, KEEP data   |   make reset   # stop and DROP this node's data
make push-certs  # OPERATOR machine only (CA_DIR holds the CA key): issue every leaf and push each node its own; no node ever holds the CA key (docs/runbooks/ca-key-off-vm2.md)
make firewall    # install and enable the host firewall (infra/node/)
```

Starting both nodes from nothing: run `make up` on vm3 (its stores come up first), then on vm2 (its ledger-service needs
vm3's Kafka), then on vm3 again (its withdrawal-service and sweeper need vm2's ledger-service). Each run is idempotent.
Whether every service retries by itself when started out of order is not verified (the design note lists it); this order
is the one used for the cutover. Secrets (database passwords, Mongo) are generated once per node into
the gitignored `.env`; there is no master key: the sealed seed in Postgres is useless without the KEK in the HSM token on vm2.

## The Hot Wallet on the HSM (optional, `SIGNER_BACKEND=pkcs11`)

By default wallet-service signs the Hot Wallet with the sealed `hot_wallet` seed. With `SIGNER_BACKEND=pkcs11` it signs
through `hsm-gateway` (SoftHSMv2, `docs/design/key-custody-hsm.md` M2b) and does not load that seed. The gateway runs on
vm2 (where the CA is), publishes `10.10.0.1:8445` only, and is pinned to wallet-service's leaf certificate; wallet-service
reaches it over WireGuard (`extra_hosts: hsm-gateway:10.10.0.1`). Nothing changes while the variable is unset.

```bash
make hsm-setup SIGNER_BACKEND=pkcs11   # vm2, once: hsm-gateway leaf, PIN, pinned caller, token + Hot Key in SoftHSM, policy
make up SIGNER_BACKEND=pkcs11          # vm2 (starts hsm-gateway first), then vm3 (wallet-service pins the address)
make verify SIGNER_BACKEND=pkcs11      # adds the HSM checks (gateway only on 10.10.0.1:8445, wallet-service on pkcs11)
```

This is a **new** Hot address with its own nonce counter. An environment that already ran on the software Hot moves its funds
and in-flight state with the cutover tooling: `make hot-cutover-freeze|settle|drain|switch|verify|complete|abort NEW_HOT=0x..`
on vm3 (`DRY_RUN=1` prints), runbook `docs/runbooks/hot-cutover.md`, rehearsed by `make -C ../e2e e2e-cutover`. After `make firewall` on vm2 the gateway port admits only vm3.
Single host (no `/etc/digiwal/node`): the same targets, the gateway is reached as `hsm-gateway:8443` on the Docker network.

## The seed vault and sweep-signer (M3c)

The user seed is a Sealed Secret in `vault.sealed_secrets`: AES-256-GCM under a per-secret DEK, the DEK wrapped by a KEK that lives only in the HSM token
on vm2. There is no master key and no software Hot Wallet in this stack (it always runs on `SIGNER_BACKEND=pkcs11`; the software baseline is `make -C ../e2e e2e`).
`make up` does, on vm3, each step only if missing: `make init` (host files, custody migrations, the roles `svc_sweepsigner` and `svc_keyadmin` and the `vault` schema
with their proofs, then `key-admin init`: generate the seed, have the gateway seal it, attest the xpub, prove address parity, activate). `sweep-signer` runs next to
custody's database and gets the seed per Sweep from `hsm-gateway` over WireGuard. Bring-up, rotation and recovery: `../docs/runbooks/dev-hsm-bringup.md` and
`../docs/runbooks/seed-rotation.md`. `make verify` section 7 checks the container (read-only, uid 10002, no capabilities, no shell, no published port, memswap == mem,
secrets as files, no key file anywhere); sections 6-12 add the same flags for hsm-gateway and wallet-service, the modes/owners of every key, PIN and password file, the audit chain
and the host settings. The simulator now runs as uid 10014 (it presents the sweeper's key, mode 0400): after pulling this change `make reset` on vm3 once, the old state volume belongs to another uid.
Alerts for all of this: `infra/prometheus/alerts.yml` group `key-custody`.

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
| DB credentials | fixed throwaway | random, generated once per node into `.env` |
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
