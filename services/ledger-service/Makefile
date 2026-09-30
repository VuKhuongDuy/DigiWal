# docker-compose.yml pulls in ../../infra/docker-compose.yml via `include:` — don't also
# pass it with -f.
COMPOSE = docker compose

.PHONY: certs up down logs test

# Issues ledger-service's leaf cert (and the CA, once). Shared with wallet-service's target.
certs:
	@test -f "$${CA_DIR:-$$HOME/.digiwal-ca}/ca-key.pem" || ../../infra/pki/gen-ca.sh
	@for svc in ledger-service withdrawal-service; do test -f ../../infra/pki/out/$$svc/cert.pem || ../../infra/pki/gen-leaf.sh $$svc; done

# The service uses its own database on the shared Postgres.
up: certs
	$(COMPOSE) up -d postgres
	@until $(COMPOSE) exec -T postgres pg_isready -U custody >/dev/null 2>&1; do sleep 1; done
	@$(COMPOSE) exec -T postgres psql -U custody -tc "SELECT 1 FROM pg_database WHERE datname='ledger'" | grep -q 1 || $(COMPOSE) exec -T postgres psql -U custody -c "CREATE DATABASE ledger"
	$(COMPOSE) up -d --build ledger-service
	@until curl -sf http://localhost:8081/healthz > /dev/null; do sleep 1; done
	@echo "up: http://localhost:8081"

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f ledger-service

# Integration tests need a real Postgres: they start one via testcontainers
# (Docker required), or use TEST_POSTGRES_DSN if set.
test:
	go test ./...
