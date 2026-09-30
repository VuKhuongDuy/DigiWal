# docker-compose.yml pulls in ../../infra/docker-compose.yml via `include:` — don't also pass it with -f.
COMPOSE = docker compose

.PHONY: certs up down logs test

# Issues api-gateway's leaf cert (and the CA, once). It calls ledger-service as CN=api-gateway.
certs:
	@test -f "$${CA_DIR:-$$HOME/.digiwal-ca}/ca-key.pem" || ../../infra/pki/gen-ca.sh
	@test -f ../../infra/pki/out/api-gateway/cert.pem || ../../infra/pki/gen-leaf.sh api-gateway

# The service uses its own database on the shared Postgres.
up: certs
	$(COMPOSE) up -d postgres
	@until $(COMPOSE) exec -T postgres pg_isready -U custody >/dev/null 2>&1; do sleep 1; done
	@$(COMPOSE) exec -T postgres psql -U custody -tc "SELECT 1 FROM pg_database WHERE datname='api_gateway'" | grep -q 1 || $(COMPOSE) exec -T postgres psql -U custody -c "CREATE DATABASE api_gateway"
	$(COMPOSE) up -d --build api-gateway
	@until curl -sf http://localhost:8084/healthz > /dev/null; do sleep 1; done
	@echo "up: http://localhost:8084"

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f api-gateway

# Integration tests need a real Postgres: testcontainers (Docker) or TEST_POSTGRES_DSN.
test:
	go test ./...
