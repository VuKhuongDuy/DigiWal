# DigiWal

A crypto custody wallet backend (deposit / withdraw / internal
transfer, EVM, **testnet only**) built to get real, hands-on experience
operating Postgres, Kafka, Redis and MongoDB — including, eventually,
running them in genuine HA topology across multiple VMs instead of a
single-machine Docker Compose stack.

Read **`docs/ARCHITECTURE.md` first** — it has the full system design,
why this domain was chosen, the security model for the one thing that
actually matters here (the master key), and the phased roadmap this
repo is being built in. `docs/TEAM_OPERATING_MODEL.md` and `CLAUDE.md`
cover how this repo is worked on (roles, process gates) — read those
before running any AI agent against this codebase.

## Status

**Phase 1 in progress**: `services/wallet-service` — generates the
master HD wallet and derives one EVM deposit address per user. Fully
working locally, see `services/wallet-service/README.md` to run it.

Not built yet: `chain-watcher`, `ledger-service`, `withdrawal-service`,
`history-service`, `api-gateway`, observability, and the multi-VM
deployment. See the roadmap in `docs/ARCHITECTURE.md` for what's next.

## Layout

```
DigiWal/
  CLAUDE.md                       repo operating rules for AI agents working here
  docs/
    ARCHITECTURE.md                system design, security model, roadmap
    TEAM_OPERATING_MODEL.md        roles, workflow, process gates
    design/                        per-feature design docs (produced before non-trivial implementation)
  infra/                          shared docker-compose services (Postgres, Redis, Kafka, MongoDB, Prometheus, Grafana)
  services/
    wallet-service/                master HD wallet + per-user deposit address derivation
    (more services land here as later phases are built)
```

## History

Formerly developed as `custody-wallet/` inside a personal notes repo
(`bytebytego`); moved here as its own repo once it outgrew being a
subfolder. Started fresh (no imported git history) — see that repo's
history if you need the earlier commit-by-commit record.
