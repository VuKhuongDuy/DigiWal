# docker-compose.yml pulls in ../../infra/docker-compose.yml via `include:` — don't also pass it with -f.
COMPOSE = docker compose

.PHONY: certs up down logs test

# Issues sweeper's leaf cert (and the CA, once). It calls wallet-service as CN=sweeper.
certs:
	@test -f "$${CA_DIR:-$$HOME/.digiwal-ca}/ca-key.pem" || ../../infra/pki/gen-ca.sh
	@for svc in sweeper wallet-service; do test -f ../../infra/pki/out/$$svc/cert.pem || ../../infra/pki/gen-leaf.sh $$svc; done

# Needs wallet-service running with both seeds initialised.
up: certs
	$(COMPOSE) up -d --build sweeper
	@until curl -sf http://localhost:8085/healthz > /dev/null; do sleep 1; done
	@echo "up: http://localhost:8085"

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f sweeper

# No database: the unit tests use a fake chain and wallet-service, plus go-ethereum's in-process EVM.
test:
	go test ./...
