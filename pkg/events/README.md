# pkg/events

Shared Go structs for the four Kafka events (`deposits.detected`, `deposits.confirmed`,
`withdrawals.broadcast`, `withdrawals.confirmed`) so producers and consumers can't drift.
Payload shapes: `docs/design/integration-contracts.md` §1. Structs + validation only.

```go
// producer
b, err := events.Encode(events.DepositConfirmed{Version: events.Version, /* ... */})
msg := kafka.Message{Topic: e.Topic(), Key: []byte(e.Key()), Value: b}

// consumer
e, err := events.Decode[events.DepositConfirmed](msg.Value) // validates; unknown version = error
```

- JSON on the wire; amounts are decimal strings (`events.Amount`), never numbers.
- Every event has `version` (currently 1). Adding an optional field keeps 1; renaming,
  removing or retyping a field bumps it. Decode ignores unknown fields.
- `Key()` is both the Kafka message key and the idempotency key to dedup on
  (deposits: `chain_id:tx_hash:log_index`, withdrawals: `withdrawal_id`, the server-minted id — the client's `idempotency_key` is unique only per user, withdrawal-flow.md invariant #20).
- `testdata/*.json` pin the exact wire shape. If a golden test fails you changed the contract.

## Using it from a service

It's its own Go module. From a service's `go.mod`:

```
require digiwal/pkg/events v0.0.0
replace digiwal/pkg/events => ../../pkg/events
```

Note: a service's Dockerfile currently builds with the service directory as the build
context, which can't see `../../pkg/events`. The first service that imports this
(chain-watcher, task 2.1) must build with the repo root as context.
