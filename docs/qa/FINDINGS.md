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

---

# Round 2 — real services after Milestone 3 (origin/main @ b74bea7)

Stack: `tests/e2e/stack2.sh up` (Dev's `e2e/docker-compose.yml`, all 7 shipped services, ports +10000, project `digiwal-qa`).
Suite: `tests/chaos` (`CHAOS=1 go test -p 1 -v ./...`), only via api-gateway, anvil RPC and docker. Ledger conservation check after every test.

## Status of round-1 findings

| ID | Round-1 finding | Now |
|---|---|---|
| D-1 | Gas Top-Up credited as deposit | **Fixed, verified** (C2: 500 TT deposit → sweeper top-up lands on the deposit address, ETH balance stays 0) |
| D-2 | Release possible after broadcast | **Not reproducible through withdrawal-service** (it never releases a lock after broadcast). Ledger itself still has no `broadcast` guard → downgraded to Medium/defence-in-depth |
| W-7 | Reorg after Settle → debited, not paid | Dev added `disputed`, Nonce Filler, Compensating Entry, Reaper. **Not re-verified by QA yet** |
| W-4 | No Nonce-Gap endpoint | Fixed (`POST /v1/wallet/nonce-gap`) — existence confirmed, flow not re-tested by QA |
| W-6 | Nonce drift never healed | Not re-tested |
| M-1 | api-gateway missing | Fixed (used by every chaos test) |
| O-1 | `make init-seed` broken | Fixed by Dev |
| K-1 | Consumer idle if topic missing at boot | **Still open** (Dev: Makefile pre-creates topics; services unchanged; no `WatchPartitionChanges`) |
| W-8 | No RPC timeout | **Still open** (no `WithTimeout` in wallet-service signing path). Effect seen in C10 |
| S-1 | No header timeouts | **Still open on 6 of 7** services (only api-gateway has `ReadHeaderTimeout`) — confirmed by `TestS1` |
| D-4 | Ledger credits any chain_id | Still open (code unchanged) |
| L-1..L-3, W-1, S-2, W-5 | probes | Not re-tested |

## New findings

| ID | Sev | Finding | Repro |
|---|---|---|---|
| **K-2** | **High** | **A Kafka restart kills ledger-service, sweeper and history-service** (`log.Fatalf("consumer: …")` on fetch error; compose has no restart policy, so they stay `Exited (1)`). ledger-service down = balances / locks / withdrawals API all return 502. Reproduced twice. A broker blip must reconnect, not crash the money API. | `TestK2_KafkaRestartDoesNotKillServices` |
| **W-9** | **High** | **A tx dropped from the mempool leaves the withdrawal in `broadcast` forever with the funds locked.** After `anvil_dropTransaction`, 150 s later: status still `broadcast`, 1 ETH locked, no re-broadcast of the stored raw tx, no failure, no alert (Fee Bump only fires for stuck *and* underpriced). When the same raw tx was re-sent it confirmed once and settled correctly, so recovery is just "someone must re-broadcast". Mempool eviction / node restart makes this a realistic production case. | `TestC12_DroppedTxIsNotLeftStuckForever` |
| C-2 | Info | Transient RPC outage (25 s) fails all in-flight withdrawals as `failed/stale_reservation` and refunds the ledger correctly (safe, no money moved, but every user has to resubmit). Wallet nonce and chain nonce stayed in sync afterwards (44 = 44). | `TestC10` |

## What the chaos run proved OK (real services, DB + chain checked)

| Case | Fault | Result |
|---|---|---|
| C1 | none (smoke) | deposit → credited → withdraw → paid, ledger total/locked correct |
| C3 | same withdrawal submitted 20× concurrently; same key different amount | 1 accepted (202), 19 replays (200), one DB row, paid once; different body → 409 |
| C4 | 20 racing withdrawals of 0.1 ETH against 1 ETH | exactly 10 accepted / 10 × 422, all 10 paid, ledger 0, never negative |
| C5 | `kill -9` withdrawal-service mid-flight (8 withdrawals) | all 8 `confirmed`, each destination exactly 0.2 ETH, ledger == chain, nothing locked |
| C6 | `kill -9` chain-watcher during 8 deposits | credited exactly 0.8 ETH (no loss, no double) |
| C7 | `kill -9` ledger-service mid-flight | 8/8 confirmed and consistent |
| C8 | Postgres restart mid-flight | 8/8 confirmed and consistent |
| C9 | Kafka restart mid-flight | 8/8 confirmed and consistent **but see K-2: three services died afterwards** |
| C10 | RPC node paused 25 s | all 8 `failed` (Reaper), 0 paid, ledger refunded exactly |
| C11 | tx in mempool, withdrawal-service killed, block mined while it is down, service restarted | `confirmed`, paid once, debited once |
| C12 (second half) | original raw tx replayed after being dropped | single payout, settle once |

Not yet exercised by QA on the real stack: reorg after settle (Dev's own e2e covers it), nonce-gap freeze/resolve flow, whitelist edge cases, history-service consistency, sweeper kills, chain-watcher reorg of deposits, ERC-20 withdrawal chaos, load.
