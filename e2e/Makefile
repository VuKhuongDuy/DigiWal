# Task 2.6 — e2e for Milestone 2 (deposit -> ledger balance) against a local Anvil chain.
#   make e2e        bring the stack up, run the tests, tear it down (volumes included)
#   make up / test / down / logs   the same steps one at a time, for debugging
COMPOSE = docker compose -p digiwal-e2e --project-directory . -f ../infra/docker-compose.yml -f docker-compose.yml
SERVICES = wallet-service ledger-service chain-watcher sweeper api-gateway
# Fixed, throwaway key: this stack's volumes are deleted on `make down`.
export MASTER_KEY_HEX ?= 0000000000000000000000000000000000000000000000000000000000000001

.PHONY: e2e certs up test down logs

e2e:
	$(MAKE) up
	$(MAKE) test; rc=$$?; $(MAKE) down; exit $$rc

certs:
	@test -f "$${CA_DIR:-$$HOME/.digiwal-ca}/ca-key.pem" || ../infra/pki/gen-ca.sh
	@for svc in $(SERVICES); do test -f ../infra/pki/out/$$svc/cert.pem || ../infra/pki/gen-leaf.sh $$svc; done

# The tests deploy the ERC-20 first thing, so a fresh chain matters: `down` always drops it.
up: certs
	$(COMPOSE) up -d --wait postgres redis kafka mongo anvil
	# Topics must exist before any consumer starts: kafka-go's group reader that joins while its
	# topic is missing never gets the partitions afterwards (found by this e2e — ledger-service and
	# history-service sat idle until restarted). Names from pkg/events.
	@for t in deposits.detected deposits.confirmed withdrawals.broadcast withdrawals.confirmed; do \
		$(COMPOSE) exec -T kafka /opt/kafka/bin/kafka-topics.sh --bootstrap-server localhost:9092 --create --if-not-exists --topic $$t --partitions 1 --replication-factor 1 || exit 1; done
	$(COMPOSE) run --rm wallet-service -init-seed user_derivation >/dev/null
	$(COMPOSE) run --rm wallet-service -init-seed hot_wallet >/dev/null
	$(COMPOSE) up -d --build --wait $(SERVICES) history-service

test:
	E2E=1 go test -count=1 -v -timeout 10m ./...

down:
	$(COMPOSE) down -v --remove-orphans

logs:
	$(COMPOSE) logs -f
