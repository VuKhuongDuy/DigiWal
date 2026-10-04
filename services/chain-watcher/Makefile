# docker-compose.yml pulls in ../../infra/docker-compose.yml via `include:` — don't also
# pass it with -f.
COMPOSE = docker compose

.PHONY: certs up down logs test

# Issues chain-watcher's leaf cert (and the CA, once). It calls wallet-service as CN=chain-watcher.
certs:
	@test -f "$${CA_DIR:-$$HOME/.digiwal-ca}/ca-key.pem" || ../../infra/pki/gen-ca.sh
	@for svc in chain-watcher wallet-service; do test -f ../../infra/pki/out/$$svc/cert.pem || ../../infra/pki/gen-leaf.sh $$svc; done

# The service uses its own database on the shared Postgres. wallet-service must be running for the
# address-resolution fallback (see ../wallet-service: `make up`, then `make init-seed`).
up: certs
	$(COMPOSE) up -d postgres
	@until $(COMPOSE) exec -T postgres pg_isready -U custody >/dev/null 2>&1; do sleep 1; done
	@$(COMPOSE) exec -T postgres psql -U custody -tc "SELECT 1 FROM pg_database WHERE datname='chain_watcher'" | grep -q 1 || $(COMPOSE) exec -T postgres psql -U custody -c "CREATE DATABASE chain_watcher"
	$(COMPOSE) up -d --build chain-watcher
	@until curl -sf http://localhost:8082/healthz > /dev/null; do sleep 1; done
	@echo "up: http://localhost:8082"

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f chain-watcher

# Integration tests need a real Postgres (testcontainers/Docker, or TEST_POSTGRES_DSN), and a
# Redis via TEST_REDIS_ADDR for the resolver tests.
test:
	go test ./...

# The auditor (dev tool) reads this service's tables directly (docs/design/reconciliation-audit.md): run
# its schema contract test so a migration here cannot silently break it. Needs Docker or TEST_POSTGRES_DSN.
.PHONY: auditor-contract
auditor-contract:
	$(MAKE) -C ../auditor contract
