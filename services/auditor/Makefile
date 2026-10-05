# docker-compose.yml pulls in ../../infra/docker-compose.yml via `include:` — don't also pass it with -f.
COMPOSE = docker compose

.PHONY: up down logs test contract roles certs grafana-role

# Issues the auditor's leaf cert (CN=auditor; wallet-service admits it on system-addresses only) and
# wallet-service's, plus the CA once.
certs:
	@test -f "$${CA_DIR:-$$HOME/.digiwal-ca}/ca-key.pem" || ../../infra/pki/gen-ca.sh
	@for svc in auditor wallet-service; do test -f ../../infra/pki/out/$$svc/cert.pem || ../../infra/pki/gen-leaf.sh $$svc; done

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

# The schema contract (what the auditor reads vs what the other services' migrations define) and the
# read-only roles. Every other service's Makefile has `auditor-contract` calling this, so a migration that
# breaks the auditor fails in that service's CI.
contract:
	go test ./internal/source -run 'TestStoreContractAndRoles' -count=1

# Create auditor_ro and its grants on the dev environment's databases (run after the services migrated;
# PSQL is a psql connected as the superuser, default: through the infra compose Postgres).
PSQL ?= $(COMPOSE) exec -T postgres psql -U custody -v ON_ERROR_STOP=1
roles:
	$(PSQL) -d postgres < roles/00_role.sql
	$(PSQL) -d ledger < roles/ledger.sql
	$(PSQL) -d custody < roles/wallet.sql
	$(PSQL) -d withdrawal < roles/withdrawal.sql
	$(PSQL) -d chain_watcher < roles/chain_watcher.sql
	$(PSQL) -d api_gateway < roles/api_gateway.sql
	@echo "MongoDB: mongosh -u custody -p custody --authenticationDatabase admin < roles/mongo.js"

# Grafana's read-only login on the auditor's own database (the Engineering dashboard's detail tables). The auditor
# must have started once, so its tables exist.
grafana-role:
	$(PSQL) -d auditor < roles/grafana.sql
