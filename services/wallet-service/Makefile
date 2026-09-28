# docker-compose.yml pulls in ../../infra/docker-compose.yml via
# `include:` — don't also pass it with -f (Compose then resolves the
# include's relative path against the first -f file's directory instead
# of the including file's, and breaks).
COMPOSE = docker compose

.PHONY: gen-key up init-master-wallet down logs test

# Generates a fresh AES-256 key into .env (gitignored). Run once per
# environment (local, staging, ...) — never reuse a key across them.
gen-key:
	@test -f .env && echo ".env already exists, not overwriting" && exit 1 || true
	@echo "MASTER_KEY_HEX=$$(openssl rand -hex 32)" > .env
	@echo "wrote .env"

up:
	$(COMPOSE) up -d --build postgres redis wallet-service
	@echo "waiting for wallet-service to be healthy..."
	@until curl -sf http://localhost:8080/healthz > /dev/null; do sleep 1; done
	@echo "up: http://localhost:8080"

# Run exactly once per environment, after `make up` — see docs/ARCHITECTURE.md's
# security notes. Prints the mnemonic to your terminal; write it down,
# it is never shown again and never stored in plaintext. Restarts the
# running service afterward so it picks up the now-available wallet
# (it only checks once, at boot).
init-master-wallet:
	$(COMPOSE) run --rm wallet-service /usr/local/bin/service -init-master-wallet
	$(COMPOSE) restart wallet-service

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f wallet-service

test:
	go test ./...
