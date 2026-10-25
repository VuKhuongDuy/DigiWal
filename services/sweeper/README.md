# sweeper

Moves confirmed deposits from a user's attribution address to the Hot Wallet (`docs/design/deposit-flow.md`,
Sweep and Gas Top-Up). It consumes `deposits.confirmed`, and for each event: screens the sender,
tops the address up with gas when it cannot pay for an ERC-20 Sweep, then sweeps. It holds **no key
material and no database** — `wallet-service` signs both transactions (mTLS, `CN=sweeper`), sweeper
decides, broadcasts and waits.

## How one event is handled

1. Sender on the **Screening List** -> skipped, never swept, never funded (`compliance-screening.md` #2; checked independently of `ledger-service`).
2. **Native coin:** re-read the balance (skip if zero: already swept) -> `POST /v1/sign/user-address` on **sweep-signer** (`SWEEP_SIGNER_URL`, key-custody-hsm.md M3b; the same request and response as when wallet-service answered it) with `amount: "max"`
   (wallet-service reserves the transaction's own fee, invariant #14, option (a)) -> broadcast -> wait until mined.
   A `422` (nothing left after gas) is dust: skipped, not an error.
3. **ERC-20:** read the token balance (skip if zero) -> if the address's native balance is below the gas reserve,
   **Gas Top-Up** (`POST /v1/sign/hot-wallet`, key `gas-topup:{chain_id}:{tx_hash}:{log_index}`) -> broadcast -> wait
   until mined -> **read the token balance again** -> sweep the *whole live balance* -> broadcast -> wait.

Nothing is remembered between attempts (idempotent by construction, invariants #8, #10): a redelivered event, a crash
after broadcast, or a retry after a timeout re-reads the chain and converges. It also refuses to sign while a
transaction from the user address is still in the mempool (pending nonce > latest), so a retry cannot stack a second
sweep behind one that is about to spend the same balance. A Sweep never touches a user's balance in the Ledger
(invariant #9); the only thing it reports is its gas (below).

**A stuck Gas Top-Up is Fee Bumped** (task 2.8, `deposit-flow.md` invariant #18, `withdrawal-flow.md` #21). A Gas
Top-Up is a Hot Wallet tx, so one that is stuck underpriced holds a Hot Wallet nonce and every withdrawal queues behind
it. While waiting for it to be mined, sweeper asks wallet-service (`GET /v1/sign/hot-wallet/{key}`, `CN=sweeper`, own
`gas-topup:…` keys only) for the key's versions, then re-signs it with `POST …/replace` — same tx, same nonce, fees
×1.125, under wallet-service's 50 gwei ceiling — when the node refuses it as underpriced, or when it has not been mined
`FEE_BUMP_AFTER_BLOCKS` (20) blocks after it was last sent and its max fee is below the current base fee. At most
`MAX_FEE_BUMPS` (5), then an `ALERT` (a refusal — the ceiling, or an operator's Nonce Filler — is an `ALERT` too, and
sweeper stops asking). Only the latest version is re-sent, every version's receipt is watched, and whichever is mined
ends the wait: a retry after a crash or timeout continues from the latest replacement instead of the original.

**Gas Fees** (`ledger.md` invariant #12): after each mined Gas Top-Up and Sweep, sweeper posts its gas to
`POST /v1/ledger/gas-fees` (`LEDGER_SERVICE_URL`, its own certificate; purpose `gas_topup` / `sweep`) with the receipt's
`gasUsed` and `effectiveGasPrice`; ledger-service computes the fee and dedupes on the tx hash, so a retry cannot double
it. For a bumped top-up it is the **mined** version's gas. Best effort, by decision (`ledger.md` Open Questions #6):
a few attempts, then an `ALERT`, and it never fails the Sweep — sweeper keeps no state, so a crash between a tx being
mined and its Gas Fee being posted loses that fee (a small drift, accepted).

Failures: **transient** (RPC/wallet-service down, revert, not mined in time) back off and retry; after `MAX_ATTEMPTS` (10)
the record is given up on with an `ALERT` log line and committed, so one stuck address cannot halt every later sweep.
**Permanent** (unknown asset, wrong chain, wallet-service `400/403/404/409`, undecodable event) is alerted and committed at once.
Giving up is safe for funds: the deposit stays credited in the ledger and stays at the user address, and the next deposit
event for that address sweeps its whole balance.

## Configuration

| Env | Default | |
|---|---|---|
| `KAFKA_BROKERS`, `RPC_URL`, `ASSETS_FILE`, `SCREENING_FILE`, `WALLET_SERVICE_URL`, `SWEEP_SIGNER_URL`, `LEDGER_SERVICE_URL`, `TLS_CERT_FILE`, `TLS_KEY_FILE`, `TLS_CA_FILE` | required | |
| `GAS_TOPUP_WEI` | `1000000000000000` | 0.001 ETH (decided, `deposit-flow.md`) |
| `SWEEP_GAS_RESERVE_WEI` | `650000000000000` | below this an ERC-20 address gets a top-up: 65,000 gas x 10 gwei, the design's own arithmetic |
| `INCLUSION_TIMEOUT` / `POLL_INTERVAL` | `5m` / `3s` | wait for a broadcast tx to be mined |
| `MAX_ATTEMPTS` | `10` | transient attempts per record |
| `FEE_BUMP_AFTER_BLOCKS` / `MAX_FEE_BUMPS` | `20` / `5` | Fee Bump of a stuck Gas Top-Up (decided) |

## Please confirm / known gaps

- The Tech Lead approved the live nonce read and the `gas-topup:...` key (`deposit-flow.md` Open Questions #7, #8), and the
  Hot Wallet address is fetched from `wallet-service` (`GET /v1/wallet/hot-address`, `CN=sweeper`) once at startup, retried for
  two minutes; there is no address in config to drift (invariant #15).
- **The "wait for pending" check and waiting for inclusion are dev additions** (not in the design), for the reasons above.
- **No dead-letter topic, by decision** (`deposit-flow.md` invariant #16, `integration-contracts.md` §5): a given-up record leaves an `ALERT` log line and can be re-driven by hand.
- Processing is serial: each event waits for its transactions to be mined (seconds on Anvil, ~12 s per block on Sepolia).
- A native sweep leaves a little dust: wallet-service reserves the fee *cap*, not the expected fee.
- The Kafka consumer loop is the third copy of the same code (`ledger-service`, `history-service`); a shared package is deferred.
- Runs under Docker, Kafka and Anvil in `e2e/` (task 2.6: native and ERC-20 deposits are swept into the Hot Wallet; make -C e2e e2e). Not run against Sepolia. Unit-level, it was run against: the sweep logic against a fake chain and wallet-service
  (mutation-checked), the chain adapter against go-ethereum's in-process EVM (real transfers, receipts, mempool nonces, `balanceOf`),
  and the wallet client against an HTTP stub. The wallet client was also run once (throwaway check, not committed) against the real
  `wallet-service` binary over mTLS with a mock RPC: top-up signed by the Hot Wallet and identical on replay, token and native-`max` sweeps signed by
  the user's derived key, wrong destination `403`, unknown user `404`, `CN=chain-watcher` refused.

## Metrics

`GET /metrics` on its own internal port (`METRICS_PORT`, default 9100). sweeper has no database ("keeps no state"), so these are **process
metrics that restart with it** (fine: in-flight work is short-lived and a restart re-consumes Kafka): `digiwal_sweeper_inflight{kind}` and
`..._inflight_oldest_age_seconds{kind}` (`gas_topup`, `sweep`; the age is read at scrape time), `digiwal_sweeper_completion_seconds{kind}` (sent to
mined, successful ones only), `digiwal_sweeper_fee_bumps_total{kind}`, `digiwal_sweeper_alerts_total{kind}` (`bump_exhausted`, `bump_refused`,
`cancelled_by_operator`, `gas_fee_report_failed`) and `digiwal_sweeper_events_abandoned_total` (records given up on; funds stay at the user address).
They come from `sweep.Observer`, a hook that only receives facts. See `pkg/metrics`.
