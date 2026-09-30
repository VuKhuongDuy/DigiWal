# e2e — Milestone 2 (task 2.6)

Deposit → balance, against a real local chain. One compose project (`docker-compose.yml`, layered
over `infra/docker-compose.yml`) runs Anvil plus the shipped containers of `wallet-service`,
`chain-watcher`, `ledger-service`, `sweeper`, `history-service` and `api-gateway`. The Go test drives
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

## Other things this run found

- Consumers that start before their topic exists never receive it (ledger-service and history-service
  sat idle until restarted). `make up` pre-creates the four topics as a workaround; the services
  themselves are unchanged.
- `services/wallet-service`'s `make init-seed` passed `/usr/local/bin/service` as an argument to an image
  whose ENTRYPOINT is already `service`, so it started the server instead of initialising the seed. Fixed.
- The gateway → ledger-service mTLS call, which the api-gateway README says was never run against the real
  binary, works.
