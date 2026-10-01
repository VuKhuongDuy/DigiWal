# docker-compose.yml pulls in ../../infra/docker-compose.yml via `include:` — don't also pass it with -f.
COMPOSE = docker compose

.PHONY: certs up down logs test

# Issues withdrawal-service's leaf cert (and the CA, once), plus api-gateway's: the only caller.
certs:
	@test -f "$${CA_DIR:-$$HOME/.digiwal-ca}/ca-key.pem" || ../../infra/pki/gen-ca.sh
	@for svc in withdrawal-service api-gateway; do test -f ../../infra/pki/out/$$svc/cert.pem || ../../infra/pki/gen-leaf.sh $$svc; done

# The service uses its own database on the shared Postgres.
up: certs
	$(COMPOSE) up -d postgres
	@until $(COMPOSE) exec -T postgres pg_isready -U custody >/dev/null 2>&1; do sleep 1; done
	@$(COMPOSE) exec -T postgres psql -U custody -tc "SELECT 1 FROM pg_database WHERE datname='withdrawal'" | grep -q 1 || $(COMPOSE) exec -T postgres psql -U custody -c "CREATE DATABASE withdrawal"
	$(COMPOSE) up -d --build withdrawal-service
	@until curl -sf http://localhost:8086/healthz > /dev/null; do sleep 1; done
	@echo "up: http://localhost:8086 (mTLS :8446)"

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f withdrawal-service

# Store tests need a real Postgres: testcontainers (Docker) or TEST_POSTGRES_DSN.
test:
	go test ./...
