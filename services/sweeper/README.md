# sweeper

Moves confirmed deposits from a user's attribution address to the Hot Wallet (`docs/design/deposit-flow.md`,
Sweep and Gas Top-Up). It consumes `deposits.confirmed`, and for each event: screens the sender,
tops the address up with gas when it cannot pay for an ERC-20 Sweep, then sweeps. It holds **no key
material and no database** — `wallet-service` signs both transactions (mTLS, `CN=sweeper`), sweeper
decides, broadcasts and waits.

## How one event is handled

1. Sender on the **Screening List** -> skipped, never swept, never funded (`compliance-screening.md` #2; checked independently of `ledger-service`).
2. **Native coin:** re-read the balance (skip if zero: already swept) -> `POST /v1/sign/user-address` with `amount: "max"`
   (wallet-service reserves the transaction's own fee, invariant #14, option (a)) -> broadcast -> wait until mined.
   A `422` (nothing left after gas) is dust: skipped, not an error.
3. **ERC-20:** read the token balance (skip if zero) -> if the address's native balance is below the gas reserve,
   **Gas Top-Up** (`POST /v1/sign/hot-wallet`, key `gas-topup:{chain_id}:{tx_hash}:{log_index}`) -> broadcast -> wait
   until mined -> **read the token balance again** -> sweep the *whole live balance* -> broadcast -> wait.

Nothing is remembered between attempts (idempotent by construction, invariants #8, #10): a redelivered event, a crash
after broadcast, or a retry after a timeout re-reads the chain and converges. It also refuses to sign while a
transaction from the user address is still in the mempool (pending nonce > latest), so a retry cannot stack a second
sweep behind one that is about to spend the same balance. A Sweep never touches the Ledger (invariant #9).

Failures: **transient** (RPC/wallet-service down, revert, not mined in time) back off and retry; after `MAX_ATTEMPTS` (10)
the record is given up on with an `ALERT` log line and committed, so one stuck address cannot halt every later sweep.
**Permanent** (unknown asset, wrong chain, wallet-service `400/403/404/409`, undecodable event) is alerted and committed at once.
Giving up is safe for funds: the deposit stays credited in the ledger and stays at the user address, and the next deposit
event for that address sweeps its whole balance.

## Configuration

| Env | Default | |
|---|---|---|
| `KAFKA_BROKERS`, `RPC_URL`, `ASSETS_FILE`, `SCREENING_FILE`, `WALLET_SERVICE_URL`, `TLS_CERT_FILE`, `TLS_KEY_FILE`, `TLS_CA_FILE` | required | |
| `GAS_TOPUP_WEI` | `1000000000000000` | 0.001 ETH (decided, `deposit-flow.md`) |
| `SWEEP_GAS_RESERVE_WEI` | `650000000000000` | below this an ERC-20 address gets a top-up: 65,000 gas x 10 gwei, the design's own arithmetic |
| `INCLUSION_TIMEOUT` / `POLL_INTERVAL` | `5m` / `3s` | wait for a broadcast tx to be mined |
| `MAX_ATTEMPTS` | `10` | transient attempts per record |

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
