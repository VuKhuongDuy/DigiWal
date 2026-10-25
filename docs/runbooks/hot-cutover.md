# Runbook: Hot cutover (software Hot key -> HSM Hot key)

Design: `docs/design/key-custody-hsm.md` 5.5 and invariant 24. Tool: `services/wallet-service/cmd/hot-cutover` (image stage `cutover`, never in
the wallet-service image). Rehearsed locally with `make -C e2e e2e-cutover`. Every step below is safe to re-run; add `DRY_RUN=1` to any
`make hot-cutover-*` to print what it would do. Run in a maintenance window. Commands are `make -C dev ...` on **vm3** unless it says vm2.

**Preconditions.** vm2 and vm3 run this commit (migration 000006 applied by wallet-service; `ledger-service` redeployed so CN `hot-cutover`
may post gas-fees; `make push-certs` on vm2 so vm3 has the `hot-cutover` leaf). No Nonce Gap open. The old Hot holds native gas for the drain
(one ERC-20 transfer, about 65k gas, per token it holds, plus a 21k native transfer, at up to the 50 gwei Fee Ceiling; the dry run states the need). `hsm-gateway` serving. Write down `NEW_HOT`.

| # | Step | Command | Notes |
|---|------|---------|-------|
| 1 | **vm2**: ceremony, gateway up | `make hsm-setup SIGNER_BACKEND=pkcs11` then `make up SIGNER_BACKEND=pkcs11` | prints the new Hot address (`dev/hsm/hot-address`). Policy pins it and the Warm address |
| 2 | Preflight, stop sweeper, freeze | `make hot-cutover-freeze NEW_HOT=0x..` | refuses unless the gateway serves and its key == `NEW_HOT`. New signs now answer 503 `hot_cutover`; replays, Fee Bump, Void, Cancel still work with the old key |
| 3 | Settle | `make hot-cutover-settle NEW_HOT=0x..` | rebroadcasts signed-but-unmined txs (incl. an in-flight Gas Top-Up), waits for all of them to be confirmed. A tx stuck below the base fee: let withdrawal-service Fee Bump it or `service hot-wallet cancel --key K`, then re-run |
| 4 | Dry run of the drain | `make hot-cutover-drain DRY_RUN=1 NEW_HOT=0x..` | amounts, gas, destination. Check the destination is `NEW_HOT` |
| 5-7 | **POINT OF NO RETURN**: drain | `make hot-cutover-drain NEW_HOT=0x..` | signs with the old key every registered ERC-20, then native last (old ends at 0), stores, broadcasts, waits, posts each tx's gas to the ledger (`purpose treasury_sweep`, key `gas:<chain>:<hash>`), and the gas of the old address's Gas Top-Ups that were in flight while the sweeper was stopped (`gas_topup`). Re-run to resume; late arrivals get another round |
| 8 | Switch | `make hot-cutover-switch NEW_HOT=0x..` | wallet-service recreated with `SIGNER_BACKEND=pkcs11`, then chain-watcher, withdrawal-service restarted. Sweeper stays stopped |
| 8 | Verify | `make hot-cutover-verify NEW_HOT=0x..` | old address empty, new funded, drains confirmed and in the ledger, nonce rows, wallet-service pinned `NEW_HOT` |
| 9 | Unfreeze, start sweeper | `make hot-cutover-complete NEW_HOT=0x..` | refuses unless drained and verified. Restart the auditor (dev) so it learns the new Hot |
| 10 | Legacy seed | after 30 days | `hot_wallet` seed stays sealed (late Sweeps in flight). Retire after a zero-balance check of the old address (`hot-cutover-verify`) |

**After step 9, check:** auditor has no open warn/critical finding; `GET /v1/wallet/hot-address` consumers log the new address; a small
withdrawal confirms with `from == NEW_HOT`.

**Rollback.** *Before step 5* (state `frozen`): `make hot-cutover-abort NEW_HOT=0x..` unfreezes, restarts the sweeper; the old key is
still the Hot key, nothing moved. *From step 5 on* only forward: the drain is signed and stored, re-running `drain` finishes it. If
the drain tx reverts or the ledger post fails the tool stops and says why; fix and re-run. A wrong `NEW_HOT` is caught at step 2 (before
any signature). If wallet-service on `pkcs11` cannot start after the drain, fix the gateway and retry `hot-cutover-switch`: funds
are on the new address and there is **no** return to the software key (the policy forbids sending to the old address).

**Differences from the design note's list:** step 4 (nonce-state migration) is migration 000005, already deployed; the freeze lives in the
`hot_cutover` table, not an HTTP route; the point of no return is the *signing* of the drain (stored, then broadcast), one instant earlier than the broadcast.
