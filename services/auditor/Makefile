# docker-compose.yml pulls in ../../infra/docker-compose.yml via `include:` — don't also pass it with -f.
COMPOSE = docker compose

.PHONY: up down logs test

# Dev environment only. The service uses its own database on the shared Postgres.
up:
	$(COMPOSE) up -d postgres anvil
	@until $(COMPOSE) exec -T postgres pg_isready -U custody >/dev/null 2>&1; do sleep 1; done
	@$(COMPOSE) exec -T postgres psql -U custody -tc "SELECT 1 FROM pg_database WHERE datname='auditor'" | grep -q 1 || $(COMPOSE) exec -T postgres psql -U custody -c "CREATE DATABASE auditor"
	$(COMPOSE) up -d --build auditor
	@until curl -sf http://localhost:8087/healthz > /dev/null; do sleep 1; done
	@echo "up: http://localhost:8087"

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f auditor

# Store tests need a real Postgres (testcontainers/Docker, or TEST_POSTGRES_DSN); they skip without one.
test:
	go test ./...
