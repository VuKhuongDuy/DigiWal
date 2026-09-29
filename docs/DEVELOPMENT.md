# Local development

Day-to-day development and testing runs against a **local Anvil node**,
not real Sepolia — this was already the plan in `docs/ARCHITECTURE.md`'s
phased roadmap ("`chain-watcher` against a local Anvil/Hardhat EVM
testnet"). Real Sepolia (confirmation depths, gas prices decided in
`docs/design/deposit-flow.md`/`withdrawal-flow.md`) is for the
demo/staging environment, closer to production conditions — not for
every local test run.

Anvil ships with [Foundry](https://getfoundry.sh) and speaks real
Ethereum JSON-RPC, so every service talks to it exactly like it would
talk to Sepolia — only the RPC URL and Confirmation Depth config change
between environments (both are already config values in the design
docs, never hardcoded).

## Install (for `cast`/`forge` — still used locally even though `anvil` itself runs in Docker below)

```bash
curl -L https://foundry.paradigm.xyz | bash
foundryup
```

## Run a local node

Anvil now runs as a proper `infra/docker-compose.yml` service (the
`ghcr.io/foundry-rs/foundry` image, whose CLI entrypoint runs `anvil`
directly) instead of a manually-run separate process — this keeps the
local chain reproducible and scriptable, matching how e2e tests
(`docs/IMPLEMENTATION_PLAN.md` tasks 2.6, 3.8) need to bring it up and
tear it down the same way as every other infra dependency:

```bash
docker compose -f infra/docker-compose.yml up anvil
```

(or just `docker compose -f infra/docker-compose.yml up` to bring up
the whole stack, which now includes `anvil` alongside postgres/redis/
kafka/mongo/prometheus/grafana).

Same defaults as running `anvil` directly: RPC at `http://127.0.0.1:8545`
(published from the container), chain id `31337`, 10 pre-funded accounts
with 10,000 ETH each (private keys printed to the container's stdout on
startup — `docker compose logs anvil` to see them) — usually enough on
its own without needing to mint more. State is in-memory and ephemeral
by design: `docker compose down`/restarting the `anvil` service gives
you a fresh chain, which is what the snapshot/revert workflow below
already assumes.

If you don't want the dependency on Docker for a quick one-off session,
running Anvil directly still works exactly as before (`foundryup` once,
then `anvil` in a terminal) — just make sure nothing else is bound to
`8545` if you also have the compose service running. The rest of this
doc (the `cast` walkthroughs, snapshot/revert) is identical either way,
since both put Anvil at the same `localhost:8545`.

Point any service at it the same way you'd point it at Sepolia:

```bash
RPC_URL=http://127.0.0.1:8545
```

For a fast dev loop, override Confirmation Depth / Withdrawal
Confirmation Depth to something small (e.g. `1`–`2` blocks) instead of
Sepolia's decided 32/64 — no reason to wait for "real" finality against
a chain you fully control.

## Unlimited test funds (no faucet needed)

Mint ETH to any address instantly:

```bash
cast rpc anvil_setBalance 0xYourTestAddress 0x56BC75E2D63100000   # 100 ETH, in wei hex
```

For ERC-20 testing, deploy a throwaway test token once per session:

```bash
forge create src/TestToken.sol:TestToken \
  --constructor-args "TestToken" "TT" 18 \
  --private-key <one of Anvil's printed private keys> \
  --rpc-url http://127.0.0.1:8545
```

(Any minimal OpenZeppelin-based ERC20 works — mint yourself a large
supply in the constructor, then transfer chunks to test deposit
addresses via `cast send`.)

## Stress testing / benchmarking

Anvil mines on demand and has no real network latency or consensus
overhead, so throughput is limited by your machine, not the chain — set
`--block-time <seconds>` for a fixed cadence, or leave it on
instant-mine (default) and drive load with k6 (per
`docs/ARCHITECTURE.md` roadmap step 5's tooling).

To fast-forward past a Confirmation Depth without waiting:

```bash
cast rpc anvil_mine 32
```

## Simulating a reorg (testing Nonce Gap recovery)

This is the one scenario that's nearly impossible to trigger
deliberately on real Sepolia, and it's exactly what
`withdrawal-flow.md`'s invariants #7–9 (Nonce Gap / `disputed` state)
exist for. Anvil's snapshot/revert makes it straightforward:

```bash
cast rpc anvil_snapshot                    # returns a snapshot id, e.g. "0x1"
# ... broadcast and mine a withdrawal tx, let it reach "confirmed" ...
cast rpc anvil_revert 0x1                  # rewinds the chain — that tx, and
                                            # everything mined after it, vanishes
```

The withdrawal that was `confirmed` a moment ago is now missing from
the canonical chain — the exact Nonce Gap condition the design's
detection/recovery loop is meant to catch.

## When to actually use Sepolia

Before calling a feature done, run it once against real Sepolia with
the decided confirmation depths (32/64 blocks) and real (if slow)
finality — Anvil proves the logic is correct, Sepolia proves it holds
up against real network conditions (variable block time, real gas
price movement, an RPC provider you don't control).
