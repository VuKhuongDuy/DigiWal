# QA notes — Milestone 0 (foundations) and Milestone 1 (ledger core)

Tested at `origin/main` @ `7ac5042`, role: tester. Nothing in application code was changed;
the only additions are the `qa_probe_test.go` files listed at the bottom.

## Verdict

- **M1 (`ledger-service`): functionally sound, ships with 3 medium and a few low findings.** All 1.1–1.8 deliverables exist, the whole suite passes against a real Postgres, and the money invariants (sum-to-zero, overdraft on Available, idempotent replay, exactly-once Lock resolution, atomic Settle, append-only entries, NUMERIC(78,0)) all held up under adversarial probing.
- **M0: not done.** Tasks 0.1–0.6 are implemented and green; **task 0.7 (`api-gateway`: mint user + API key, Bearer-key middleware) does not exist anywhere** — no `services/api-gateway`, on any branch. M0's own DoD ("`api-gateway` can mint a user + API Key and reject any request without a valid one") is therefore unmet. `IMPLEMENTATION_PLAN.md` still lists it "Not started", and the same table still lists `ledger-service` as "Not started" although it is merged — the status table is stale.

## What was run

`go test ./... -count=1` in `pkg/events`, `services/wallet-service`, `services/ledger-service` (Go 1.26 container, testcontainers → real `postgres:16-alpine`): **all packages pass.** wallet-service also mines its signed txs in a real EVM, and its signature-recovery test (plan's required test) is present and asserts the recovered address equals the Hot Wallet.

Not run: docker-compose end-to-end smoke, real Kafka, Redis-backed paths beyond the unit tests, mTLS handshake against the real `infra/pki` certs (handshake is covered by `authz_test.go`, not by a live listener).

## Findings (ranked)

### Blocker
**M-1. Task 0.7 `api-gateway` is missing** (see Verdict). Every later `api-gateway` task (2.5, 3.7) is blocked behind it. Either build it or move it out of M0 explicitly.

### Medium
**L-1. One idempotency-key namespace shared by deposits, corrections and Settle.** Settle inserts `ledger_transactions` with the *Lock's* key, and that column is `UNIQUE`. `ledger.md` says "no collision to namespace against" — true between two Settles, false against a deposit key (`chainId:txHash:logIndex`) or a correction key. Ledger does not validate key format, so it trusts `withdrawal-service` to only send UUIDs.
- Key already used by another tx → `Settle` fails with a raw unique-violation (HTTP 500), the Lock stays `active`, the withdrawal that was already broadcast can never be debited (Release would hand the money back → user keeps funds that left the chain).
- Reverse order → a settled withdrawal's key blocks the real deposit with that key; consumer parks it in `failed_events` and the depositor is never credited.
- Repro: `TestQA_SettleKeyAlreadyUsedByAnotherTransaction_DoesNotWedgeTheLock`, `TestQA_SettledWithdrawalKeyBlocksALaterDepositWithTheSameKey`.
- Fix (dev, or SA if the doc must change): namespace Settle's key (e.g. store `source_type` in the unique index) or enforce UUID for Lock keys and a disjoint format for the rest.

**L-2. `Apply(source_type=deposit)` does not enforce deposit shape.** Any balanced entry set is accepted: `alice -1000 / bob +1000` posts as a "deposit" and moves funds user→user with no Lock and no chain leg. Not reachable from the HTTP surface today (handler rejects non-correction; consumer hard-codes user+/chain−), so this is defense-in-depth — but `Apply` is the invariant boundary and M2+ code will call it. Repro: `TestQA_DepositShapeIsNotEnforced`. Fix: a deposit must be exactly one positive user entry + one equal negative chain entry.

**L-3. `SyncAssets` silently redefines an asset that already has balances.** Changing `decimals`, `symbol`, `asset_type` or `contract_address` for an existing id in `infra/assets.yaml` is upserted at next boot with no check, reinterpreting every stored balance. Repro: `TestQA_SyncAssetsRefusesToRedefineAnAssetThatHasBalances`. Fix: refuse to change identity fields (`decimals`, `asset_type`, `contract_address`) once the asset has accounts.

**W-1. `wallet-service` signs amounts above uint256.** `POST /v1/sign/hot-wallet` with `amount = 2^256` returns a signed tx for both native and ERC-20; the ERC-20 calldata is malformed (33-byte amount word, because `LeftPadBytes` doesn't truncate) and the tx consumes a real nonce. If ever broadcast it can never be mined → Nonce Gap → freeze. Caller is a trusted service so exposure is low, but a stuck nonce on the Hot Wallet is exactly what invariant #7 exists to avoid. Repro: `TestQA_AmountAboveUint256IsRejected`. Fix: reject `amount > 2^256-1` in `validate`. (The ledger side already has this check.)

### Low
- **L-4.** NUL byte in `idempotency_key`/`user_id` → Postgres `invalid byte sequence` → HTTP 500 instead of 400. `TestQA_NULByteInIdentifiersIsA400NotA500`.
- **L-5.** API: lock keys `.` / `..` can't be settled (ServeMux 307 redirect); a body with trailing JSON after the object is accepted (`Decoder` never checks for EOF); amount `"+5"` is accepted as 5. Cosmetic today because withdrawal-service sends UUIDs.
- **L-6.** Consumer parks unknown-asset / unknown-user deposits in `failed_events` and commits the offset. That is a deliberate, documented choice, but real on-chain funds sit there with no replay tool and no design for one (the code comment says so). Needs an SA decision before M2 puts real deposits through it.
- **W-2.** `wallet-service` will sign a transfer to the zero address or to the Hot Wallet itself. Destination policy belongs to `withdrawal-service`; noting so it lands in M3's test list.
- **W-3.** Plan/doc drift: `IMPLEMENTATION_PLAN.md` module map still says `wallet-service` "no signing endpoint" and `ledger-service` "Not started".

### Probed, held up (no finding)
Concurrent Locks never over-reserve; concurrent Settle/Release resolve exactly once; Settle atomic under injected mid-tx failure; Release posts no Entry; correction can't mint, can't half-reverse, can't undercut an active Lock, correction-of-a-correction round-trips exactly; entries append-only incl. TRUNCATE; amounts beyond int64 exact; two uint256-max deposits still fit NUMERIC(78,0); deactivating an asset doesn't strand in-flight Locks; hot-wallet nonces dense/unique under concurrency, failed signing burns no nonce, freeze refuses new but replays stored keys; same key + different request rejected.

## Test files added (tests only)

`services/ledger-service/internal/ledger/qa_probe_test.go`, `.../api/qa_probe_test.go`, `services/wallet-service/internal/signing/qa_probe_test.go`. Tests for open findings carry `t.Skip("open finding X-n …")` so CI stays green; Dev removes the skip when fixing. The rest (asset deactivation, correction round-trip, amount-format and destination probes) run and pass.
