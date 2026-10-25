# DigiWal — QA test plan (digital wallet, EVM)

Owner: Tester. Scope: create account, deposit, withdraw, internal transfer, plus the cross-cutting
ledger / security / operational properties. Written from the design docs in `docs/design/` and from
reading the code, then attacked as a wallet tester would: *where can money be created, lost, duplicated
or locked forever, and where can the system stop making progress?*

## 1. Non-negotiable properties (what "broken" means here)

| # | Property | Sev if violated |
|---|---|---|
| P1 | **No double credit / double debit.** A deposit event, a withdrawal request or a Settle applied N times (retry, redelivery, concurrency, crash-and-replay) moves money exactly once. | Critical |
| P2 | **No negative balance.** `available = total - locked >= 0` for every user account, always, under any interleaving. | Critical |
| P3 | **Conservation.** Per asset: sum of all entries = 0; `total_balance` = sum of that account's entries; `locked_balance` = sum of its active locks; chain account = -(sum of user totals). | Critical |
| P4 | **Solvency.** Sum of user balances never exceeds what the custody addresses actually hold on chain (hot + user addresses + in-flight). Money is never credited that did not arrive, and never released while it may still leave. | Critical |
| P5 | **No signing outside policy.** Hot Wallet key signs only for authorised callers; user keys only sweep to the Hot Wallet; never two different txs for one nonce/idempotency key. | Critical |
| P6 | **Liveness.** No operation can wedge the system: no lock stuck `active` forever, no partition blocked behind one bad event, no DB/RPC call that hangs without a timeout, no nonce gap left unnoticed. | High |
| P7 | **Crash safety.** Killing any process or the DB at any step leaves a state that recovers without manual SQL and without P1–P5 violations. | High |
| P8 | **Auditability.** Every balance change has an immutable entry; every failure is parked/visible, not dropped. | High |

Severity: **Critical** = money lost/created or key compromise · **High** = stuck system / wrong state needing manual repair · **Medium** = wrong error/response, degraded safety margin · **Low** = cosmetic.

## 2. Status legend

- `AUTO` — automated by QA e2e suite (`tests/e2e`, real stack: Postgres, Redis, Kafka, anvil, both services).
- `UNIT` — already covered by Dev's own tests (QA re-ran, assertions reviewed).
- `PROBE` — QA probe test in the service package (`qa_probe_test.go`).
- `BLK-M2` / `BLK-M3` — needs `chain-watcher`/`sweeper` (M2) or `withdrawal-service` (M3); in the e2e suite a **harness stand-in** drives the same contract (marked *harness*), so ledger/wallet side is still tested.
- `BLK-GW` — needs `api-gateway` (task 0.7, missing).
- `BLK-DESIGN` — no design doc yet; case is the acceptance list for the doc.
- `MANUAL` — exploratory / needs human judgement.

## 3. Environment

`anvil` (chain 31337, pre-funded accounts, `--block-time` optional) · Postgres 16 (DBs `custody`, `ledger`) · Redis · Kafka · `wallet-service` + `ledger-service` from `docker compose`, mTLS with real `infra/pki` certs.
Anvil levers used: `anvil_mine`, `evm_snapshot`/`evm_revert` (reorg), `anvil_setBalance`, `anvil_setNonce`, `anvil_dropTransaction`, `evm_setAutomine(false)` / `evm_setIntervalMining` (pending/stuck tx), `anvil_setNextBlockBaseFeePerGas` (gas spike), `anvil_setCode` (fake/hostile tokens), `anvil_impersonateAccount`.
After **every** e2e test the *invariant checker* (section 9) runs against Postgres; a test cannot pass if P3 is broken.

---

## 4. Create account

| ID | Scenario | Expected | Sev | Status |
|---|---|---|---|---|
| ACC-01 | `GET/POST deposit-address` twice for same user | same address, one `wallet_addresses` row | High | UNIT+AUTO |
| ACC-02 | 50 concurrent first-calls for the **same** user | exactly one address & one index; no 500s | Critical (two addresses = funds on an address nobody maps) | AUTO |
| ACC-03 | 100 concurrent calls for **distinct** users | indices dense & unique, addresses unique, `next_index` = N | Critical | UNIT+AUTO |
| ACC-04 | Address independently recomputed from seed + `derivation_index` (QA holds test mnemonic) | equals DB address; never equal to Hot Wallet address | Critical | AUTO |
| ACC-05 | Crash between index allocation and address insert (kill service mid-burst) | no duplicate index, no address row without index, retry yields one address; a skipped index is acceptable | High | AUTO (chaos) |
| ACC-06 | Redis flushed / down | address & `GET /v1/addresses/{a}` still answer from Postgres; Redis repopulated; no 500 | High | AUTO |
| ACC-07 | Redis holds stale/wrong `user_id` for an address | design says Redis is a cache; Postgres wins — **verify which actually wins** | High | AUTO |
| ACC-08 | Hostile `user_id`: empty, 10 kB, unicode, `../`, `'; DROP`, NUL, whitespace-only, case variants (`Alice`/`alice`) | 4xx, never 500, no SQL effect; case variants are *distinct users* (confirm intended) | Medium | AUTO |
| ACC-09 | `init-seed` run twice for a role | second refused, first seed untouched | Critical | UNIT |
| ACC-10 | Service started with **no** hot-wallet seed / no RPC | `/v1/sign/*` → 503, never signs with the wrong seed; deposit-address still works | Critical | AUTO |
| ACC-11 | `user_derivation` seed ≡ `hot_wallet` seed (operator mistake) | refused, or at minimum Hot Wallet address ≠ any user address (else sweeping = signing as hot wallet) | Critical | AUTO |
| ACC-12 | Wrong / missing key material (legacy software key), tampered sealed seed | fails closed at boot, no partial service | High | UNIT+AUTO |
| ACC-13 | Mint user + API key (`POST /users`) returns key once; DB stores only hash; bad/missing/rotated key → 401; client-sent `user_id` ignored | — | Critical | BLK-GW |
| ACC-14 | Ledger account lazily created on first deposit; 20 concurrent first deposits for a brand-new user | exactly one account row, balances sum correct | Critical | AUTO |
| ACC-15 | Same user, ETH and token → two accounts; locks/balances independent | — | High | UNIT |

## 5. Deposit

Harness note: until M2 lands the e2e suite contains a **stand-in chain-watcher** (scans anvil blocks, resolves user via `GET /v1/addresses/{a}` over mTLS, publishes `deposits.confirmed` to Kafka in the exact `pkg/events` shape) and a **stand-in sweeper** (gas top-up + sweep through the real wallet endpoints). Cases marked *(real M2)* must be re-run against the real services when they exist.

| ID | Scenario | Expected | Sev | Status |
|---|---|---|---|---|
| DEP-01 | Native deposit on anvil → event → ledger | one tx, 2 entries (+user, −chain), `total_balance`=amount, chain acct=−amount; on-chain balance of address = amount | Critical | AUTO |
| DEP-02 | Same event published 5× sequentially | credited once; `already_applied` | Critical (P1) | UNIT+AUTO |
| DEP-03 | Same event, 20 consumers/goroutines racing | credited once | Critical | UNIT+AUTO |
| DEP-04 | Consumer killed after `Apply` commit, before offset commit → restart | redelivered, credited once | Critical | AUTO (chaos) |
| DEP-05 | Consumer killed **before** apply | redelivered, credited once, nothing lost | Critical | AUTO (chaos) |
| DEP-06 | Ledger Postgres stopped 20 s while events arrive, then restarted | events retried (backoff), all credited exactly once, none parked, no manual action | High (P6) | AUTO (chaos) |
| DEP-07 | ERC-20 deposit (deploy token on anvil) | credited in token asset account; log_index ≥ 0 | Critical | AUTO |
| DEP-08 | One tx, two Transfer logs to two users / same user | two ledger txs, distinct log_index | Critical | UNIT+AUTO |
| DEP-09 | Native + ERC-20 in same tx hash | distinct keys (`-1` vs log index) | High | AUTO |
| DEP-10 | Amount 1 wei; 10^30; uint256 max | exact, no float/int64 truncation | High | UNIT |
| DEP-11 | Zero-value transfer (address-poisoning spam) | no credit, parked or ignored, no account created, partition not wedged | High | UNIT+AUTO |
| DEP-12 | Unregistered ERC-20 (airdrop spam / fake "USDC") | no credit, no account; visible (parked/metric) | Critical (P4) | AUTO |
| DEP-13 | Fee-on-transfer token: log says X, address received < X | **ledger must not credit more than was received** — decide: token not allowed in registry, or credit by balance delta | Critical (P4) | AUTO+DESIGN |
| DEP-14 | Hostile token: `transfer` returns false / reverts / emits Transfer without moving funds (`anvil_setCode`) | event with no real funds must not be credited if token is unregistered; for registered token only trusted tokens listed | Critical | AUTO |
| DEP-15 | Deposit to an address with no owner (not in `wallet_addresses`) | not credited; parked; address lookup 404 handled | High | UNIT+AUTO |
| DEP-16 | Event for another chain id / wrong asset id | rejected (`unsupported chain`/`unknown asset`), no credit | Critical | AUTO |
| DEP-17 | **Same natural key, different amount** (re-emitted after bug/reorg) | `ErrIdempotencyKeyReused` → parked + alert; original credit untouched | Critical | UNIT+AUTO |
| DEP-18 | Sender on Screening List (also mixed-case address) | flagged not credited; one flag even if redelivered 10× ; operator list reload | High | UNIT+AUTO |
| DEP-19 | Confirmation depth: deposit with <32 confs | **not** in `deposits.confirmed` | Critical (P4) | BLK-M2 (stand-in enforces) |
| DEP-20 | Reorg within depth (`evm_snapshot`→deposit→`evm_revert`) | deposit never confirmed, never credited | Critical | BLK-M2 / AUTO with stand-in |
| DEP-21 | Reorg deeper than depth after credit | **out of scope in design (deposit-flow open Q #4)** — ledger must at least allow a correction; add reconciliation alarm | Critical | BLK-DESIGN |
| DEP-22 | Out-of-order: `confirmed` before `detected`, duplicates of `detected` | no effect on ledger | Medium | BLK-M2 |
| DEP-23 | Poison record (invalid JSON / wrong version) between good ones | parked, offset advances, later records processed | High (P6) | UNIT+AUTO |
| DEP-24 | **Gas Top-Up is not a deposit.** Hot Wallet → user address 0.001 ETH (native). Does chain-watcher see it as a deposit to that user? | must be excluded (sender = Hot Wallet / our own addresses). Otherwise user is credited phantom funds that the sweep later returns to the hot wallet → custody drained | **Critical** | **DESIGN GAP** — not addressed in `deposit-flow.md`; AUTO test written to demonstrate with stand-in |
| DEP-25 | Sweep native `max`: dest = Hot Wallet only; value = balance − gas cap; dust remains; tx mined; hot wallet +value | `to` is never caller-controlled | Critical | UNIT+AUTO |
| DEP-26 | Sweep twice in a row (retry) | second sweep reads live balance, nothing/dust only; nothing double-moved | High | AUTO |
| DEP-27 | Second deposit lands between balance read and sign | swept next round; no loss; no double-credit | High | AUTO |
| DEP-28 | Gas Top-Up idempotency: replay key → same signed tx, one nonce, one on-chain top-up | — | High | UNIT+AUTO |
| DEP-29 | Sweep tx underpriced / stuck (automine off) then replaced | no second sweep of same funds; nonce correct (live pending nonce) | High | AUTO |
| DEP-30 | Dust deposit < sweep gas | credited (ledger) but never swept; reported, not retried forever | Medium | AUTO |
| DEP-31 | ERC-20 sweep without prior top-up (no gas) | `estimateGas`/send fails cleanly, no nonce burned on hot wallet | High | AUTO |
| DEP-32 | Deposit event `user_id` ≠ owner of `to_address` | ledger trusts event (by design) — check chain-watcher cross-checks | High | BLK-M2 |

## 6. Withdraw

Harness stand-in for `withdrawal-service` (lock → sign → broadcast → wait confirm → settle/release) uses the real mTLS endpoints with `CN=withdrawal-service`.

| ID | Scenario | Expected | Sev | Status |
|---|---|---|---|---|
| WDR-01 | Happy path ETH: balance 1.0, withdraw 0.4 | lock active (avail 0.6) → sign → on-chain recipient +0.4, hot wallet −0.4−gas → settle: total 0.6, locked 0, entries −0.4/+0.4, chain acct, lock `settled`, `ledger_transactions` `withdrawal_settle` | Critical | AUTO |
| WDR-02 | Happy path ERC-20 | token moves, calldata `transfer(to,amount)` to registered contract | Critical | AUTO |
| WDR-03 | Withdraw > available / == available / available+1 wei | 422 / OK / 422; balance never negative | Critical (P2) | UNIT+AUTO |
| WDR-04 | Withdraw while another lock is active (uses *available*, not total) | rejected if it would exceed available | Critical | UNIT+AUTO |
| WDR-05 | Amount 0, negative, `+5`, `1e18`, 2^256, huge digits, below `min_withdrawal_amount` | 400/422, nothing locked; min-withdrawal enforced (by withdrawal-service) | High | UNIT+AUTO |
| WDR-06 | Same idempotency key twice (sequential) | one lock, one signed tx, one on-chain tx | Critical (P1) | UNIT+AUTO |
| WDR-07 | Same key, 30 concurrent | one lock, one nonce, identical response; `hot_wallet_signatures` 1 row | Critical | UNIT+AUTO |
| WDR-08 | Same key, different amount/address | 409, nothing changes | Critical | UNIT+AUTO |
| WDR-09 | 40 concurrent distinct withdrawals, balance fits only 12 | exactly 12 succeed; available ≥ 0; sum(locked)=12×amount | Critical (P2) | UNIT+AUTO |
| WDR-10 | Concurrent withdraw + correction + settle on one account | no negative, no deadlock (lock-order) | Critical | AUTO |
| WDR-11 | Concurrent Settle × Release on same lock | exactly one resolution | Critical | UNIT+AUTO |
| WDR-12 | Settle replay 10× | one debit | Critical | UNIT+AUTO |
| WDR-13 | Release after Settle / Settle after Release | 409, no balance change | Critical | UNIT+AUTO |
| WDR-14 | Settle/Release unknown key | 404 | Medium | UNIT |
| WDR-15 | **Crash after lock, before sign** | lock stays active until resolver releases; no on-chain tx | High (P6) | AUTO (chaos) |
| WDR-16 | **Crash after sign, before broadcast** | recovery re-broadcasts the *same stored* raw tx (idempotent replay), never re-signs with a new nonce | Critical | AUTO |
| WDR-17 | **Crash after broadcast, before settle** | lock still active; recovery sees tx mined → settle; user not able to spend that money meanwhile | Critical (P4) | AUTO |
| WDR-18 | Crash after settle, before ack/event | replay of settle idempotent | High | AUTO |
| WDR-19 | **Release while the signed tx may still be mined** (broadcast timed out, tx in mempool, then mined) | must not release until tx is provably dropped/replaced; otherwise user keeps money AND receives funds → **double spend** | **Critical** | AUTO (anvil automine off → mine later) + DESIGN check |
| WDR-20 | Broadcast rejected (nonce too low / underpriced / insufficient funds) | classified correctly; no settle; release only when no tx exists | Critical | AUTO |
| WDR-21 | **Nonce gap**: nonce n signed but never broadcast, n+1 broadcast | n+1 stuck pending; freeze flag raised; no new nonces issued; operator path to resolve; **alert** | High (P6) | UNIT+AUTO |
| WDR-22 | Hot wallet balance < amount (+gas) | not signed; withdrawal waits/fails visibly, lock held or released consistently; no nonce burned | High | AUTO |
| WDR-23 | Gas spike (`anvil_setNextBlockBaseFeePerGas`) above `max_gas_price` | 4xx/409 ceiling error, nothing signed, no nonce burned | High | UNIT+AUTO |
| WDR-24 | ERC-20 withdrawal, hot wallet lacks tokens | `estimateGas` reverts → no signature, no nonce burn | High | AUTO |
| WDR-25 | Destination: zero address, hot wallet itself, a contract, address with bad checksum, 19/21-byte, no `0x` | zero/self/malformed rejected by withdrawal-service; signer must at least not crash | High | PROBE+BLK-M3 |
| WDR-26 | Destination on Screening List | withdrawal refused (compliance-screening) | High | BLK-M3 |
| WDR-27 | Withdrawal tx reorged out **after** settle | ledger debited but funds never left → needs reconciliation/re-broadcast; solvency alarm | Critical | AUTO (revert) + BLK-DESIGN |
| WDR-28 | Hot wallet below `hot_floor` / above `hot_ceiling` → tiering, sweep to cold | wallet-tiering behaviour | High | BLK-M4 |
| WDR-29 | Withdrawal while the deposit that funds it is not yet swept | hot wallet liquidity vs ledger balance; withdrawal pending, not failed | High | BLK-M3 |
| WDR-30 | Active locks older than N hours | monitored/alerted; operator tool to resolve — **no expiry mechanism designed** | High (P6) | BLK-DESIGN |
| WDR-31 | Signed tx replay after chain reorg / on another chain | chain_id in signature; other chain rejects | Critical | AUTO |
| WDR-32 | Gas estimation timeout / RPC down / RPC returns wrong chain id | 503, no nonce burned, **bounded by a timeout** | High (P6) | AUTO |
| WDR-33 | Hot wallet tx replaced externally (operator sends tx with same nonce) | seeded-once nonce drifts: next sign collides → detect (nonce too low) and heal | High | AUTO |

## 7. Internal transfer

`BLK-DESIGN` — `IMPLEMENTATION_PLAN.md` #2 and `integration-contracts.md` §1/§3 confirm no design doc, no topic, no endpoint. Acceptance list the design must satisfy (and which QA will implement as tests the day it exists):

| ID | Scenario | Expected | Sev |
|---|---|---|---|
| TRF-01 | A→B amount ≤ available | one ledger tx, 2 entries (−A,+B), sum 0, chain account untouched | Critical |
| TRF-02 | amount > available; == available; 0; negative | reject / OK / reject / reject; never negative | Critical |
| TRF-03 | Same idempotency key ×N, concurrent ×30 | applied once | Critical |
| TRF-04 | A→A (self) | rejected or no-op, never creates money | High |
| TRF-05 | Concurrent A→B and B→A, 500 each | no deadlock (ordered locking), conservation holds | Critical |
| TRF-06 | Transfer vs active withdrawal lock | uses available, can't spend locked funds | Critical |
| TRF-07 | To unknown / not-yet-created user | defined behaviour (reject or lazy-create), no orphan funds | High |
| TRF-08 | Asset mismatch (A's ETH to B's token account) | impossible by construction | Critical |
| TRF-09 | Crash mid-transfer | both legs or neither (single DB tx) | Critical |
| TRF-10 | Caller authz: only api-gateway CN; `from` always the authenticated user, never client-supplied | — | Critical |
| TRF-11 | Frozen/flagged users, transfer limits | per compliance doc | High |

Meanwhile **QA found that `Apply(source_type=deposit)` already permits user→user moves** (L-2 in `m0-m1-qa-notes.md`): the future transfer feature must not be built on top of that hole.

## 8. Cross-cutting

### 8.1 Ledger integrity (LED)
| ID | Scenario | Sev | Status |
|---|---|---|---|
| LED-01 | Invariant checker (section 9) after every test | Critical | AUTO |
| LED-02 | Randomised concurrent workload: 1 000 ops (deposit / lock / settle / release / correction / replay) × 32 workers × 5 users × 2 assets, then checker | Critical | AUTO |
| LED-03 | Corrections: reverse settle, reverse deposit while funds locked, reverse twice, reverse a correction, reverse one leg only, reverse non-existent, caller-supplied amount ignored | Critical | UNIT+AUTO |
| LED-04 | `entries` immutable (UPDATE/DELETE/TRUNCATE) | Critical | UNIT |
| LED-05 | Direct SQL attempt to break CHECKs: negative `available`, `locked>total` | High | UNIT |
| LED-06 | Migration up/down/up preserves schema; second boot is a no-op | Medium | UNIT |
| LED-07 | Settle key / deposit key namespace collision (L-1) | Medium | PROBE |
| LED-08 | Registry change under live balances (L-3) | Medium | PROBE |
| LED-09 | Solvency check: ledger chain account vs real on-chain holdings of hot + user addresses | Critical | AUTO (reconciliation script) |

### 8.2 Security (SEC)
| ID | Scenario | Sev | Status |
|---|---|---|---|
| SEC-01 | CN matrix over **real TLS** for every mTLS endpoint (wallet: hot-wallet/user-address/addresses; ledger: transactions/locks/settle/release): allowed CN, other service CN, no cert, cert from foreign CA, expired cert, self-signed with right CN | Critical | UNIT (fake TLS state) + AUTO (real handshake) |
| SEC-02 | TLS < 1.3, plain HTTP to mTLS port | High | AUTO |
| SEC-03 | Plain listener exposes only `/healthz` (try `/v1/...` on :8080/:8081) | Critical | AUTO |
| SEC-04 | `deposit-address` endpoints on wallet plain listener: unauthenticated by design? any host on the network can create addresses / enumerate users | High | AUTO (document) |
| SEC-05 | Secrets never in logs: mnemonic, seed, any key material, raw tx key; grep container logs after full run | Critical | AUTO |
| SEC-06 | Hot wallet key cannot be used via user-address endpoint (user_id → hot index) | Critical | PROBE |
| SEC-07 | Oversized bodies, deep JSON, unknown fields, duplicate keys | Medium | UNIT+AUTO |
| SEC-08 | Slowloris / stalled client on every listener — **no `ReadHeaderTimeout` set** on any `http.Server` | High (P6) | AUTO → finding |
| SEC-09 | `ledger` correction endpoint: caller-supplied account/amount ignored | Critical | UNIT |
| SEC-10 | Timing/ordering: 403 body identical for all denial reasons | Low | UNIT |

### 8.3 Operations / resilience (OPS)
| ID | Scenario | Sev | Status |
|---|---|---|---|
| OPS-01 | Restart wallet-service / ledger-service mid-load | no lost/dup money | High | AUTO |
| OPS-02 | `kill -9` ledger-service during 200 concurrent Settles; restart | invariants hold; each lock resolved once | Critical | AUTO |
| OPS-03 | Postgres restart during load; connection pool recovers | no permanent 500s | High | AUTO |
| OPS-04 | Kafka restart / partition rebalance mid-stream | no dup, no loss | High | AUTO |
| OPS-05 | RPC stalls (proxy never answers) | sign endpoint returns within bound; DB tx/pool not pinned (nonce row `FOR UPDATE` held during RPC on first use) | High (P6) | AUTO |
| OPS-06 | Slow query / long lock: second request on same account waits with no `lock_timeout` | High | AUTO |
| OPS-07 | Bad `assets.yaml` / missing fields / duplicate ids → fails startup, doesn't boot half-usable | Medium | UNIT |
| OPS-08 | Disk/connection exhaustion: > pool size concurrent requests | queue, not deadlock | High | AUTO |
| OPS-09 | Clock skew / cert expiry on rotation | Medium | MANUAL |
| OPS-10 | Metrics/alerts exist for: parked events, stuck locks, nonce gap, hot wallet low, flagged deposits | High | MANUAL |

---

## 9. Invariant checker (runs after every e2e test)

Against the `ledger` DB (and `custody` DB where relevant):

1. `SUM(entries.amount) GROUP BY asset` = 0 for every asset (P3).
2. For every account: `total_balance = SUM(entries.amount WHERE account_id=…)`.
3. For every user account: `locked_balance = SUM(locks.amount WHERE status='active')`, `total_balance - locked_balance >= 0`.
4. Chain account `total_balance = -SUM(user total_balance)` per asset.
5. Every `ledger_transactions` row has ≥2 entries summing to 0; `withdrawal_settle` ⇔ exactly one `settled` lock with same key.
6. `locks.status='settled'` ⇒ settle tx exists; `released` ⇒ no entry for that key; `active` ⇒ `resolved_at IS NULL`.
7. No deposit natural key appears twice; no `entries.corrects_entry_id` repeated.
8. wallet: `hot_wallet_signatures.nonce` unique & dense from the seed; `next_nonce` = max+1; every `wallet_addresses.derivation_index` unique and < `next_index`.
9. Solvency (when anvil is attached): `-chain_account(ETH)` ≤ balance(hot) + Σ balance(user addresses) + in-flight withdrawals.

## 10. Execution order and reporting

1. Re-run all Dev unit/integration suites (done, green @ `7ac5042`).
2. e2e smoke on live stack (ACC-01, DEP-01/02, WDR-01/03/06) — gate before anything else.
3. Critical concurrency + chaos (P1, P2, P3, P6, P7).
4. Security + resilience.
5. Blocked sets re-run when M2/M3/api-gateway/transfer ship.

Findings go to `docs/qa/FINDINGS.md` (ID, severity, repro command, expected vs actual, owner: dev / architect). Each automated test names its case ID so a failure maps directly to this table.
