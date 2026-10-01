# QA findings — M0 + M1 (wallet-service, ledger-service) on anvil

Run: 2026-10-01, `origin/main` @ `7ac5042`, fresh stack (`tests/e2e/stack.sh reset`), `E2E=1 go test ./... -p 1` in `tests/e2e`.
Result: **46 e2e tests, 35 pass, 11 fail (every failure is a finding below, none is a harness bug)**. The invariant checker
(`docs/qa/TEST_PLAN.md` §9) ran after every test and **never fired**: across ~1 300 randomised concurrent ops, kill -9 of the ledger,
Postgres restart, 150-deposit stream with the consumer killed 3×, 40-way racing withdrawals — no double credit, no double debit,
no negative balance, entries always sum to zero. The money core (Apply / Lock / Settle / Release, idempotency, nonce allocation) is solid.
What is NOT solid is everything *around* it: the seams between chain, wallet and ledger.

Severity: Critical = money created/lost · High = system stuck / manual DB surgery · Medium = degraded safety · Low = cosmetic.
Owner: **SA** = needs a design decision, **Dev** = implementation fix.

## Critical

| ID | Finding | Repro (case) | Owner |
|---|---|---|---|
| **D-1** | **Gas Top-Up is credited as a user deposit.** Hot Wallet → user address (0.001 ETH) is a native transfer to a watched address; `deposit-flow.md` defines no sender exclusion, so chain-watcher emits `deposits.confirmed` for it and the ledger credits the user. Phantom, withdrawable balance; the sweep then returns the ETH to the Hot Wallet, so custody pays out money it never received. Also applies to any Hot-Wallet-originated transfer to a user address. | `TestDEP24` (uses a stand-in watcher that follows the doc literally, so this proves the *design*, to be re-run against real chain-watcher) | SA → Dev (M2) |
| **D-2** | **Release is accepted for a withdrawal whose signed tx is still live.** Ledger has only `active/settled/released`; nothing records "broadcast". Orchestrator times out → Release → tx is mined: recipient paid *and* user keeps the balance. Observed on anvil: recipient +0.001 ETH, user ledger still 0.001 ETH. | `TestWDR19` | SA (add a `broadcast` state / forbid Release once a signature exists for the key) |
| **W-7** | **Reorg after Settle:** user debited, recipient never paid; wallet-service nonce counter (14) ahead of chain (13) → next withdrawals stuck behind a gap. Nothing in wallet/ledger detects it; the re-check loop in `withdrawal-flow.md` #7–9 is not built. | `TestWDR27` (`evm_snapshot`/`evm_revert`) | M3 must prove it; SA for the repair path (re-broadcast stored raw tx vs Compensating Entry) |

## High

| ID | Finding | Repro | Owner |
|---|---|---|---|
| **K-1** | **Deposit consumer never starts if `deposits.confirmed` does not exist when ledger-service boots** (fresh env, chain-watcher not yet run). Silent: `/healthz` green, no error log, no consumer group. After the topic appears nothing is consumed for 90 s+ (until restart). All deposits stall. (`stack.sh` now pre-creates the topic as a workaround.) | `TestOPS_K1` | Dev (kafka-go `WatchPartitionChanges` / ensure-topic at boot / readiness check tied to consumer) |
| **W-4** | **Nonce Gap freeze is unreachable.** `FreezeNonceGap`/`ResolveNonceGap` exist but have no HTTP endpoint and no caller outside tests. A gap can be neither reported nor cleared; wallet keeps signing behind a stuck nonce. Real example found by accident: tests that signed without broadcasting wedged the hot wallet (chain nonce 6, db 8). | `TestWDR21` | Dev (+ SA: who calls it) |
| **W-6** | **Nonce drift is never healed.** `next_nonce` is seeded from the RPC once. If the Hot Wallet's on-chain nonce moves (operator tx, 2nd instance, reorg) every later signature is "nonce too low" and ALL withdrawals fail until someone edits `hot_wallet_nonces`. | `TestWDR33` | Dev/SA |
| **W-8** | **No RPC timeout.** With the RPC node unresponsive (`docker pause anvil`) `POST /v1/sign/hot-wallet` did not answer within 30 s; handler goroutines pile up. | `TestOPS05` | Dev |
| **M-1** | **api-gateway (task 0.7) does not exist** → M0 DoD unmet; blocks all user-facing flows (create account, withdraw, transfer). | — | Dev / TL scope call |

## Medium

| ID | Finding | Repro | Owner |
|---|---|---|---|
| **L-1** | Settle reuses the Lock's key in the shared `ledger_transactions.idempotency_key` namespace: a colliding deposit/correction key makes Settle fail (500, lock stuck, tx already on chain) or makes a later real deposit unpostable. | `qa_probe_test.go` (ledger, skipped) | SA/Dev |
| **L-2** | `Apply(source_type=deposit)` accepts any balanced entries (user→user transfer without a Lock). Not reachable from HTTP today. | probe | Dev |
| **L-3** | `SyncAssets` silently redefines decimals/symbol/type/contract of an asset that already has balances. | probe | Dev |
| **W-1** | Hot-wallet signer signs amounts > uint256 (malformed ERC-20 calldata, burns a nonce). | probe | Dev |
| **D-4** | Ledger credits `deposits.confirmed` events for any `chain_id` (`chain_id=1` credited on a 31337 deployment): anyone able to produce to the topic can mint balance; no allowed-chain check. | `TestDEP16b` | Dev |
| **S-1** | No `ReadHeaderTimeout`/`ReadTimeout`/handshake timeout on any of the 4 listeners: idle half-open connections live forever (slowloris/FD exhaustion). | `TestSEC08` | Dev |
| **S-2** | wallet-service `deposit-address` endpoints are on the plain, unauthenticated listener: anyone reaching :8080 can create unlimited addresses (burn derivation indices, fill the table) and enumerate user ids. Known (mTLS OQ #4) — raising priority. | `TestSEC04` | SA/Dev |
| **W-5** | Sign of an unfundable ERC-20 transfer (estimateGas reverts) returns a generic 500 — the orchestrator cannot tell permanent from transient failure and may retry forever or release wrongly. | `TestWDR24` (log) | Dev |
| **O-1** | `make init-seed` (README flow) hangs: image has `ENTRYPOINT ["service"]`, Makefile passes `/usr/local/bin/service -init-seed …`, so the flag is ignored and the server starts. Correct: `docker compose run --rm wallet-service -init-seed <role>`. | found while bringing the stack up | Dev |

## Low / observations

- **A-1** Racing first-requests for one user can hit the unique-key race; saw it return 500 to the loser once in two runs (retry then succeeds; index burned). `TestACC02` is flaky-red for this reason.
- **A-2** NUL byte in `user_id` → 500 (and 10 kB user ids accepted). `TestACC08`.
- **L-4/L-5** NUL in ledger ids → 500; `.`/`..` lock keys unsettleable; trailing JSON accepted; `+5` accepted (probes).
- **L-6** Unknown-asset deposits are parked in `failed_events` with no replay tool (real on-chain funds).
- **C-1** Consumer recovery after `kill -9` takes ~30 s (session timeout); 150 deposits with 3 kills took 95 s. First consumption after a fresh boot took >79 s once (ACC-14 in the full run; not reproduced in isolation).
- **W-2** Hot wallet will sign to zero address / itself (policy belongs to withdrawal-service — M3 test list).

## What was verified OK (evidence in the suite)

Deposit credit exactly-once (20 duplicates + concurrent producers); native + ERC-20 end to end with DB rows and on-chain balances checked; unknown asset, zero amount, poison JSON, same-key-different-amount, screened sender: parked/flagged, stream keeps moving; overdraft boundaries incl. exact available and +1 wei, locked funds excluded; 40 racing withdrawals → exactly 12 succeed; 30-way retries of lock/sign/settle → one lock, one nonce, one debit; settle-vs-release race resolves once; kill -9 ledger during 60 settles → all 60 settled once; Postgres restart during deposits → none parked, none lost; native sweep only to Hot Wallet (arbitrary destination 403), value+fee+dust = 1 ETH exactly; gas top-up replay = identical tx; sign survives wallet restart with identical tx; gas ceiling refuses without burning nonce; CN matrix over real TLS for 7 CNs × 7 endpoints; foreign-CA cert, no cert, TLS 1.2, plain-HTTP rejected; plain listeners expose no money endpoint; no mnemonic/master key in logs; Redis flush harmless.

## Not testable yet (see TEST_PLAN.md)

Internal transfer (no design), real chain-watcher/sweeper/withdrawal-service (M2/M3, stand-ins used), api-gateway (missing), wallet tiering (M4).
