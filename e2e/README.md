# e2e — Milestones 2 and 3 (tasks 2.6, 3.8)

Deposit → balance (M2) and withdrawals (M3), against a real local chain. One compose project (`docker-compose.yml`, layered
over `infra/docker-compose.yml`) runs Anvil plus the shipped containers of `wallet-service`,
`chain-watcher`, `ledger-service`, `sweeper`, `history-service`, `api-gateway` and `withdrawal-service`. The Go test drives
it from outside: only through `api-gateway`'s public routes, Anvil's JSON-RPC, and (for the Hot Wallet's
address) `wallet-service`'s mTLS endpoint using the sweeper's leaf certificate.

```bash
make e2e     # certs, stack up, tests, stack down (volumes dropped). Needs Docker, Go, openssl.
make up / test / down / logs   # the same, one step at a time
```

The stack always starts on a **fresh chain**: the test deploys `contracts/MockERC20.sol` first, and
`assets.yaml` registers the address Anvil gives account #0's first contract. If `make test` says the
token landed elsewhere, run `make down up`.

## What is asserted

1. `POST /users` and `GET /v1/deposit-address` — stable address per user, distinct across users.
2. 2 ETH sent to alice's address, 500 TT sent to bob's; after 5 blocks (Confirmation Depth is 2 here)
   `GET /v1/balances` shows exactly those amounts (chain-watcher → Kafka → ledger → gateway over mTLS).
3. The sweeper empties both deposit addresses into the Hot Wallet.
4. A third user has zero balances, and nothing moves again after the sweep (no double credit).
5. The sweeper's Gas Top-Up into bob's address is **not** credited to him as a deposit (regression test for
   `deposit-flow.md` invariant #17, found by the first version of this suite).

`contracts/MockERC20.bin` is the compiled `MockERC20.sol` (`forge build`, solc 0.8.x); rebuild it only if
the contract changes.

## Withdrawals (task 3.8, `withdrawal_test.go`)

Everything goes through `api-gateway`; the chain is Anvil. Where the test needs an operator it does what the
operator would, with `docker compose exec` (the Nonce Filler, `-abandon`) and `psql` (lifting the freeze).
Deposits are credited and swept first (alice 2 ETH, carol 3 ETH, bob 500 TT), then:

1. **Whitelist.** Whitelisting a user's own deposit address (or another user's) is `422`; a new entry is
   `pending` — a withdrawal to it is rejected — and becomes `active` after the cooldown (2s here, 24h decided).
2. **Rejections leave the balance untouched:** not whitelisted, below the minimum, insufficient funds, and
   invariant #18's own-deposit-address case (an entry inserted past its cooldown, as if whitelisted before the
   rule) — each `422` with its `reject_reason`, no Lock, balance unchanged.
3. **Native and ERC-20 withdrawals** (`202` → broadcast → 2 confirmations → Settled): the destination holds
   exactly the amount, the tx on chain is from the Hot Wallet, the ledger shows the debit with nothing locked,
   a duplicate key is the same request and a different body under it is `409`, nothing is paid twice.
4. **Fee Bump.** Anvil's automine is switched off so the tx waits in the pool; 22 blocks at a base fee above
   its cap make it stuck *and* underpriced, so withdrawal-service has wallet-service re-sign it at the same
   nonce. It confirms with the replacement's hash; the original is never mined; one payout.
5. **A forced Nonce Gap** (`evm_snapshot` before, `evm_revert` after a confirmed and Settled withdrawal):
   the request becomes `disputed` and wallet-service freezes Hot Wallet signing at that nonce; a withdrawal
   made during the freeze waits in `reserved` and the **Reaper** reclaims it by itself (void → Release,
   `stale_reservation`; timeouts are 20s here, 5 minutes decided); the operator runs wallet-service's
   `hot-wallet cancel` (Nonce Filler) and `service -abandon`, which posts the **Compensating Entry**: carol's
   balance is whole again; the freeze is lifted in the database and a new withdrawal confirms.

The reorg test is last because it makes chain-watcher's view of the reverted blocks stale; it uses native
deposits only, since a reverted Gas Top-Up would be a second nonce gap.

## Other things this run found

- Consumers that start before their topic exists never receive it (ledger-service and history-service
  sat idle until restarted). `make up` pre-creates the four topics as a workaround; the services
  themselves are unchanged.
- `services/wallet-service`'s `make init-seed` passed `/usr/local/bin/service` as an argument to an image
  whose ENTRYPOINT is already `service`, so it started the server instead of initialising the seed. Fixed.
- A node says a tx's cap is too low in more than one way. Anvil (and geth's state transition) answer `max fee
  per gas less than block base fee`, which withdrawal-service did not recognise as "underpriced", so a Fee
  Bump was not triggered for it. Fixed in `pipeline.underpriced` (unit-tested; the e2e Fee Bump is triggered by
  the stuck-and-underpriced rule, since the node-refusal case needs the base fee to rise between signing and
  broadcast).
- The gateway → ledger-service mTLS call, which the api-gateway README says was never run against the real
  binary, works.

## `sim` — drive the stack by hand (`cmd/sim`)

A CLI to simulate users, deposits and withdrawals against the running stack (`make up` first). It goes through
`api-gateway` and Anvil only. Users (their API keys are shown once) are saved in `~/.digiwal-sim/state.json`.

```bash
go build -o bin/sim ./cmd/sim
bin/sim init                          # deploy the test token, float the Hot Wallet (other commands do this on demand)
bin/sim user create alice             # or: user create -n 5
bin/sim deposit alice eth 2           # on-chain send, mines past the Confirmation Depth, waits for the ledger credit
bin/sim deposit bob tt 500
bin/sim withdraw alice eth 0.5 [--to 0x...]   # whitelists the destination (random if omitted), waits until confirmed
bin/sim user list                     # deposit addresses + live available/locked balances
bin/sim mine 10                       # the sweeper/chain-watcher only advance when blocks are mined
bin/sim reset                         # forget saved users (do this after `make down up`)
```

The sim leaves the chain used (token deployed at nonce 0, account #0 has sent txs): run `make down up` before
`make test`, which expects a fresh chain.
