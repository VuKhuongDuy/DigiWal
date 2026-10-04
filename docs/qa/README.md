# Running the QA suite

```
tests/e2e/stack.sh reset          # fresh anvil + Postgres + Redis + Kafka + wallet-service + ledger-service (ports +10000, compose project digiwal-qa)
E2E=1 go test -p 1 -v ./...        # from tests/e2e  (Go 1.26; talks to the stack, checks DB after every test)
tests/e2e/stack.sh down
```
Needs Docker, openssl. Test CA, master key and mnemonics live in `tests/e2e/.state` (gitignored, test material only).
`E2E` unset => suite skips. Failing tests are open findings (docs/qa/FINDINGS.md); they go green when fixed.
Stand-ins for unbuilt services (chain-watcher, sweeper, withdrawal-service) are in `harness_test.go` / `wdr_test.go`; replace with the real services in M2/M3.

## Chaos suite on the real services (Round 2)
```
tests/e2e/stack2.sh up            # Dev's e2e compose (7 services) on ports +10000, project digiwal-qa
CHAOS=1 go test -C tests/chaos -p 1 -v ./...
tests/e2e/stack2.sh down
```
Faults are injected with `docker kill/pause/restart` against `digiwal-qa-*` containers only. Do not run against another stack.
