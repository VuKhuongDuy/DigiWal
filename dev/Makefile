# The always-on dev environment: the whole stack + a simulator generating user activity.
#   make up        start (or update) this node's part; safe to re-run
#   make down      stop and remove containers, KEEP the data
#   make reset     stop and DROP this node's data (databases, chain, simulated users) — a clean slate
#   make status / logs / sim-logs / users / verify / firewall
#
# Two modes. With /etc/digiwal/node containing vm2 or vm3 the node's overlay (nodes/<NODE>.yml) is layered on
# and only that node's services run (docs/design/multi-node-deployment.md); without it everything runs on this
# host, as before. Same host ports as the e2e stack in single-host mode, so `make -C ../e2e down` first.
NODE ?= $(shell cat /etc/digiwal/node 2>/dev/null)
FILES = -f ../infra/docker-compose.yml -f ../e2e/docker-compose.yml -f docker-compose.yml
ifneq ($(NODE),)
FILES += -f nodes/$(NODE).yml
endif
COMPOSE = docker compose -p digiwal-dev --project-directory ../e2e --env-file .env $(FILES)

ifeq ($(NODE),vm2)
# e2e/docker-compose.yml interpolates MASTER_KEY_HEX for wallet-service even where it is switched off; the real key
# exists only on vm3 (invariant 9), so vm2 passes a placeholder that no running container receives.
export MASTER_KEY_HEX := not-on-this-node
STORES   = postgres mongo
SERVICES = ledger-service api-gateway history-service
else ifeq ($(NODE),vm3)
STORES   = postgres redis kafka anvil
SERVICES = wallet-service withdrawal-service chain-watcher sweeper simulator
else
STORES   = postgres redis kafka mongo anvil
SERVICES = wallet-service ledger-service chain-watcher sweeper api-gateway withdrawal-service history-service simulator
endif
TOPICS = deposits.detected deposits.confirmed withdrawals.broadcast withdrawals.confirmed withdrawals.failed

.PHONY: up down reset status logs sim-logs users certs push-certs env roles firewall verify

# Secrets are generated once and kept in the gitignored .env, per node. The master key exists only where
# wallet-service runs (vm3, or the single host): the sealed seeds in Postgres are useless without it, and
# losing it means losing every address. REDIS_PASSWORD is generated on vm3 (it hosts Redis); vm2 copies it from there.
env:
	@touch .env; chmod 600 .env
	@gen() { grep -q "^$$1=" .env || { echo "$$1=$$(openssl rand -hex 32)" >> .env; echo "generated $$1"; }; }; \
	case "$(NODE)" in \
	vm2) for v in PG_ADMIN_PASSWORD SVC_LEDGER_PASSWORD SVC_GATEWAY_PASSWORD SVC_HISTORY_PASSWORD MONGO_ADMIN_PASSWORD; do gen $$v; done; \
		grep -q '^REDIS_PASSWORD=' .env || { pw=$$(ssh vm3 "sed -n 's/^REDIS_PASSWORD=//p' /root/DigiWal/dev/.env" 2>/dev/null); \
		echo "$$pw" | grep -Eq '^[0-9a-f]{64}$$' || { echo "REDIS_PASSWORD: vm3 has none yet; run 'make env' on vm3 first (vm2 copies it from there)"; exit 1; }; \
		echo "REDIS_PASSWORD=$$pw" >> .env; echo "copied REDIS_PASSWORD from vm3"; } ;; \
	vm3) for v in PG_ADMIN_PASSWORD SVC_WALLET_PASSWORD SVC_WITHDRAWAL_PASSWORD SVC_CHAINWATCHER_PASSWORD REDIS_PASSWORD MASTER_KEY_HEX; do gen $$v; done ;; \
	*) gen MASTER_KEY_HEX ;; \
	esac

# The CA lives on vm2 only (~/.digiwal-ca); vm3 receives just the leaf directories of its own services.
certs:
ifeq ($(NODE),vm3)
	@for s in wallet-service withdrawal-service chain-watcher sweeper; do test -f ../infra/pki/out/$$s/cert.pem || { echo "missing leaf for $$s: run 'make push-certs' on vm2"; exit 1; }; done
else
	$(MAKE) -C ../e2e certs
	@chmod 700 ../infra/pki/out
endif

push-certs: certs
	@test "$(NODE)" = vm2 || { echo "run on vm2: it holds the CA"; exit 1; }
	ssh vm3 'mkdir -p /root/DigiWal/infra/pki/out && chmod 700 /root/DigiWal/infra/pki/out'
	@for s in wallet-service withdrawal-service chain-watcher sweeper; do \
		rsync -a --delete ../infra/pki/out/$$s/ vm3:/root/DigiWal/infra/pki/out/$$s/ || exit 1; done

firewall:
	@test -f /etc/digiwal/firewall.env || { echo "create /etc/digiwal/firewall.env with PEER_PUBLIC_IP=..."; exit 1; }
	install -m 755 ../infra/node/firewall.sh /usr/local/sbin/digiwal-firewall
	install -m 644 ../infra/node/digiwal-firewall.service /etc/systemd/system/digiwal-firewall.service
	systemctl daemon-reload && systemctl enable --now digiwal-firewall.service

roles:
ifeq ($(NODE),vm2)
	@. ./.env; docker exec -i -e ADMIN_PW=$$MONGO_ADMIN_PASSWORD -e HISTORY_PW=$$SVC_HISTORY_PASSWORD digiwal-dev-mongo-1 sh -s < ../infra/mongo/roles.sh
else
	@echo "nothing to do on this node (Postgres roles are created when the data directory is first initialised)"
endif

up: env certs
	$(COMPOSE) up -d --wait $(STORES)
ifneq ($(NODE),vm2)
	@for t in $(TOPICS); do \
		$(COMPOSE) exec -T kafka /opt/kafka/bin/kafka-topics.sh --bootstrap-server localhost:9092 --create --if-not-exists --topic $$t --partitions 1 --replication-factor 1 || exit 1; done
	@# Seeds are generated once per environment (init is a no-op when they exist, but there is no reason to run it).
	@n=$$($(COMPOSE) exec -T postgres psql -U custody -d custody -tAc "SELECT count(*) FROM wallet_seeds" 2>/dev/null || echo 0); \
	if [ "$${n:-0}" -lt 2 ]; then \
		$(COMPOSE) run --rm wallet-service -init-seed user_derivation >/dev/null && \
		$(COMPOSE) run --rm wallet-service -init-seed hot_wallet >/dev/null && echo "wallet seeds created"; \
	fi
else
	$(MAKE) roles
endif
	$(COMPOSE) up -d --build --wait $(SERVICES)

down:
	$(COMPOSE) down --remove-orphans

reset:
	$(COMPOSE) down -v --remove-orphans

status:
	$(COMPOSE) ps --format 'table {{.Service}}\t{{.Status}}\t{{.Ports}}'

logs:
	$(COMPOSE) logs -f --tail=100 $(SVC)

sim-logs:
	$(COMPOSE) logs -f --tail=100 simulator

users:
	$(COMPOSE) exec simulator sim user list

verify:
	@NODE=$(NODE) ./verify.sh
