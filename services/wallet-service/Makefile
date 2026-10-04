# docker-compose.yml pulls in ../../infra/docker-compose.yml via
# `include:` — don't also pass it with -f (Compose then resolves the
# include's relative path against the first -f file's directory instead
# of the including file's, and breaks).
COMPOSE = docker compose

.PHONY: gen-key certs up init-seeds down logs test

# Generates a fresh AES-256 key into .env (gitignored). Run once per
# environment (local, staging, ...) — never reuse a key across them.
gen-key:
	@test -f .env && echo ".env already exists, not overwriting" && exit 1 || true
	@echo "MASTER_KEY_HEX=$$(openssl rand -hex 32)" > .env
	@echo "wrote .env"

# Creates the project CA once (key stays in ~/.digiwal-ca, never in the repo or a
# container) and issues the leaf certs the signing path uses. Safe to re-run:
# an existing CA is kept, missing leaf certs are issued (delete infra/pki/out/<svc> to rotate). See docs/design/service-auth-mtls.md.
certs:
	@test -f "$${CA_DIR:-$$HOME/.digiwal-ca}/ca-key.pem" || ../../infra/pki/gen-ca.sh
	@for svc in wallet-service ledger-service chain-watcher withdrawal-service sweeper api-gateway; do test -f ../../infra/pki/out/$$svc/cert.pem || ../../infra/pki/gen-leaf.sh $$svc; done

up: certs
	$(COMPOSE) up -d --build postgres redis anvil wallet-service
	@echo "waiting for wallet-service to be healthy..."
	@until curl -sf http://localhost:8080/healthz > /dev/null; do \
		$(COMPOSE) ps --status running --services | grep -qx wallet-service || { echo "wallet-service exited — see: $(COMPOSE) logs wallet-service" >&2; exit 1; }; \
		sleep 1; done
	@echo "up: http://localhost:8080"

# Run exactly once per role per environment, after `make up` — see
# docs/design/wallet-tiering.md invariant #1: the user_derivation seed and
# the hot_wallet seed are separate. Each prints its mnemonic to your
# terminal once; write it down, it is never shown again and never stored
# in plaintext. Restarts the running service afterward so it picks up the
# new seed (it only checks at boot).
#   make init-seed ROLE=user_derivation
#   make init-seed ROLE=hot_wallet
init-seed:
	@test -n "$(ROLE)" || (echo "usage: make init-seed ROLE=user_derivation|hot_wallet" && exit 1)
	$(COMPOSE) run --rm wallet-service -init-seed $(ROLE)
	$(COMPOSE) restart wallet-service

init-seeds:
	$(MAKE) init-seed ROLE=user_derivation
	$(MAKE) init-seed ROLE=hot_wallet

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f wallet-service

# Storage tests need a real Postgres: they start one via testcontainers
# (Docker required), or use TEST_POSTGRES_DSN if set.
test:
	go test ./...

# The auditor (dev tool) reads this service's tables directly (docs/design/reconciliation-audit.md): run
# its schema contract test so a migration here cannot silently break it. Needs Docker or TEST_POSTGRES_DSN.
.PHONY: auditor-contract
auditor-contract:
	$(MAKE) -C ../auditor contract
