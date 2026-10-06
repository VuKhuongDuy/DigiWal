# The always-on dev environment: the whole stack + a simulator generating user activity.
#   make up        start (or update) this node's part; safe to re-run
#   make down      stop and remove containers, KEEP the data
#   make reset     stop and DROP this node's data (databases, chain, simulated users) — a clean slate
#   make status / logs / sim-logs / users / verify / firewall
#
# Two modes. With /etc/digiwal/node containing vm2, vm3 or vm4 the node's overlay (nodes/<NODE>.yml) is layered on
# and only that node's services run (docs/design/multi-node-deployment.md); without it everything runs on this
# host, as before. Same host ports as the e2e stack in single-host mode, so `make -C ../e2e down` first.
NODE ?= $(shell cat /etc/digiwal/node 2>/dev/null)
FILES = -f ../infra/docker-compose.yml -f ../e2e/docker-compose.yml -f docker-compose.yml
ifneq ($(NODE),)
FILES += -f nodes/$(NODE).yml
endif
COMPOSE = docker compose -p digiwal-dev --project-directory ../e2e --env-file .env $(FILES)

ifneq ($(filter $(NODE),vm2 vm4),)
# e2e/docker-compose.yml interpolates MASTER_KEY_HEX for wallet-service even where it is switched off; the real key
# exists only on vm3 (invariant 9), so vm2 passes a placeholder that no running container receives.
export MASTER_KEY_HEX := not-on-this-node
ifeq ($(NODE),vm2)
STORES   = postgres mongo
SERVICES =
MONITORING = node-exporter cadvisor postgres-exporter
else
STORES   =
SERVICES = ledger-service api-gateway history-service withdrawal-service
MONITORING = node-exporter cadvisor
endif
else ifeq ($(NODE),vm3)
STORES   = postgres redis kafka anvil
SERVICES = wallet-service chain-watcher sweeper simulator
MONITORING = node-exporter cadvisor postgres-exporter kafka-exporter alertmanager prometheus grafana
else
STORES   = postgres redis kafka mongo anvil
SERVICES = wallet-service ledger-service chain-watcher sweeper api-gateway withdrawal-service history-service simulator
MONITORING =
endif
TOPICS = deposits.detected deposits.confirmed withdrawals.broadcast withdrawals.confirmed withdrawals.failed

.PHONY: up down reset status logs sim-logs users certs push-certs env roles audit-roles monitoring firewall verify

# Secrets are generated once and kept in the gitignored .env, per node. The master key exists only where
# wallet-service runs (vm3, or the single host): the sealed seeds in Postgres are useless without it, and
# losing it means losing every address.
env:
	@touch .env; chmod 600 .env
	@gen() { grep -q "^$$1=" .env || { echo "$$1=$$(openssl rand -hex 32)" >> .env; echo "generated $$1"; }; }; \
	case "$(NODE)" in \
	vm2) for v in PG_ADMIN_PASSWORD SVC_LEDGER_PASSWORD SVC_GATEWAY_PASSWORD SVC_HISTORY_PASSWORD MONGO_ADMIN_PASSWORD EXPORTER_PASSWORD; do gen $$v; done ;; \
	vm4) for v in SVC_LEDGER_PASSWORD SVC_GATEWAY_PASSWORD SVC_HISTORY_PASSWORD SVC_WITHDRAWAL_PASSWORD; do grep -q "^$$v=" .env || { echo "$$v missing: copy it from vm2's .env (the databases' passwords are set there)"; exit 1; }; done ;; \
	vm3) for v in PG_ADMIN_PASSWORD SVC_WALLET_PASSWORD SVC_WITHDRAWAL_PASSWORD SVC_CHAINWATCHER_PASSWORD SVC_AUDITOR_PASSWORD EXPORTER_PASSWORD MASTER_KEY_HEX; do gen $$v; done ;; \
	*) gen MASTER_KEY_HEX ;; \
	esac

# The CA lives on vm2 only (~/.digiwal-ca); vm3 receives just the leaf directories of its own services.
certs:
ifeq ($(NODE),vm4)
	@for s in ledger-service api-gateway withdrawal-service; do test -f ../infra/pki/out/$$s/cert.pem || { echo "missing leaf for $$s: run 'make push-certs' on vm2"; exit 1; }; done
else ifeq ($(NODE),vm3)
	@for s in wallet-service chain-watcher sweeper auditor; do test -f ../infra/pki/out/$$s/cert.pem || { echo "missing leaf for $$s: run 'make push-certs' on vm2"; exit 1; }; done
else
	$(MAKE) -C ../e2e certs
	@test -f ../infra/pki/out/auditor/cert.pem || ../infra/pki/gen-leaf.sh auditor
	@chmod 700 ../infra/pki/out
endif

push-certs: certs
	@test "$(NODE)" = vm2 || { echo "run on vm2: it holds the CA"; exit 1; }
	ssh vm3 'mkdir -p /root/DigiWal/infra/pki/out && chmod 700 /root/DigiWal/infra/pki/out'
	@for s in wallet-service chain-watcher sweeper auditor; do \
		rsync -a --delete ../infra/pki/out/$$s/ vm3:/root/DigiWal/infra/pki/out/$$s/ || exit 1; done
	ssh vm4 'mkdir -p /root/DigiWal/infra/pki/out && chmod 700 /root/DigiWal/infra/pki/out'
	@for s in ledger-service api-gateway withdrawal-service; do \
		rsync -a --delete ../infra/pki/out/$$s/ vm4:/root/DigiWal/infra/pki/out/$$s/ || exit 1; done

firewall:
	@test -f /etc/digiwal/firewall.env || { echo "create /etc/digiwal/firewall.env with PEER_PUBLIC_IP=..."; exit 1; }
	install -m 755 ../infra/node/firewall.sh /usr/local/sbin/digiwal-firewall
	install -m 644 ../infra/node/digiwal-firewall.service /etc/systemd/system/digiwal-firewall.service
	systemctl daemon-reload && systemctl enable --now digiwal-firewall.service

# Postgres: one role and database per entry of PG_DB_ROLES (idempotent, so a database added later appears on a node that
# already has data) and the exporter's pg_monitor login; vm2 also the Mongo users.
roles:
ifeq ($(NODE),vm4)
	@echo "nothing to do on vm4: it has no database"
else
	$(COMPOSE) exec -T postgres sh /docker-entrypoint-initdb.d/10-init-node.sh
	@. ./.env; { echo "\\set pw '$$EXPORTER_PASSWORD'"; cat ../infra/postgres/exporter.sql; } | $(COMPOSE) exec -T postgres psql -q -U custody -v ON_ERROR_STOP=1 -d postgres
ifeq ($(NODE),vm2)
	@. ./.env; docker exec -i -e ADMIN_PW=$$MONGO_ADMIN_PASSWORD -e HISTORY_PW=$$SVC_HISTORY_PASSWORD digiwal-dev-mongo-1 sh -s < ../infra/mongo/roles.sh
endif
endif

# The auditor's read-only login on the databases of THIS node (services/auditor/roles); the services must have migrated.
AUDIT_ROLES = ../services/auditor/roles
PSQL = $(COMPOSE) exec -T postgres psql -q -U custody -v ON_ERROR_STOP=1
# Each database revokes CONNECT from PUBLIC (infra/postgres/init-node.sh), so the login is granted it explicitly.
audit-roles:
ifeq ($(NODE),vm4)
	@echo "nothing to do on vm4: it has no database"
else
	$(PSQL) -d postgres < $(AUDIT_ROLES)/00_role.sql
ifeq ($(NODE),vm2)
	$(PSQL) -d postgres -c "GRANT CONNECT ON DATABASE ledger TO auditor_ro"
	$(PSQL) -d ledger < $(AUDIT_ROLES)/ledger.sql
else
	$(PSQL) -d postgres -c "GRANT CONNECT ON DATABASE custody, withdrawal, chain_watcher TO auditor_ro"
	$(PSQL) -d custody < $(AUDIT_ROLES)/wallet.sql
	$(PSQL) -d withdrawal < $(AUDIT_ROLES)/withdrawal.sql
	$(PSQL) -d chain_watcher < $(AUDIT_ROLES)/chain_watcher.sql
endif
endif

# The auditor (vm3) and the Measurement Plane. Started after the services: the auditor needs their tables and, on vm3,
# vm2's ledger and Mongo roles (run `make up` on vm2 first), and Grafana's login needs the auditor's own tables.
monitoring: audit-roles
ifeq ($(NODE),vm3)
	$(COMPOSE) up -d --build --wait auditor
	$(PSQL) -d auditor < $(AUDIT_ROLES)/grafana.sql
endif
	$(COMPOSE) up -d --wait $(MONITORING)

up: env certs
ifneq ($(STORES),)
	$(COMPOSE) up -d --wait $(STORES)
endif
ifneq ($(NODE),)
	$(MAKE) roles
endif
ifeq ($(filter $(NODE),vm2 vm4),)
	@for t in $(TOPICS); do \
		$(COMPOSE) exec -T kafka /opt/kafka/bin/kafka-topics.sh --bootstrap-server localhost:9092 --create --if-not-exists --topic $$t --partitions 1 --replication-factor 1 || exit 1; done
	@# Seeds are generated once per environment (init is a no-op when they exist, but there is no reason to run it).
	@n=$$($(COMPOSE) exec -T postgres psql -U custody -d custody -tAc "SELECT count(*) FROM wallet_seeds" 2>/dev/null || echo 0); \
	if [ "$${n:-0}" -lt 2 ]; then \
		$(COMPOSE) run --rm wallet-service -init-seed user_derivation >/dev/null && \
		$(COMPOSE) run --rm wallet-service -init-seed hot_wallet >/dev/null && echo "wallet seeds created"; \
	fi
endif
ifneq ($(SERVICES),)
	$(COMPOSE) up -d --build --wait $(SERVICES)
endif
ifneq ($(NODE),)
	$(MAKE) monitoring
endif

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
