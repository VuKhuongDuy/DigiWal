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
FILES = -f ../infra/docker-compose.yml -f ../e2e/docker-compose.yml -f docker-compose.yml -f ../e2e/docker-compose.hsm.yml
# Since key-custody-hsm.md M3c this stack always runs on the HSM: hsm-gateway holds the KEK that seals the user_derivation seed (the vault) and
# the Hot Key that signs the Hot Wallet, so there is no software key and no master key anywhere in it. The gateway runs where the CA is (vm2, or
# the single host), never on vm3. A stack that still wants the old software Hot Wallet (the regression baseline) is `make -C ../e2e e2e`.
# Runbook: ../docs/runbooks/dev-hsm-bringup.md.
export SIGNER_BACKEND ?= pkcs11
ifneq ($(SIGNER_BACKEND),pkcs11)
$(error dev/ runs on the HSM only (SIGNER_BACKEND=pkcs11): the software Hot Wallet and its master key live in ../e2e, see docs/runbooks/dev-hsm-bringup.md)
endif
export HSM_DIR ?= $(abspath hsm)
ifeq ($(filter $(NODE),vm3 vm4),)
export COMPOSE_PROFILES := hsm
HSM_SERVICES = hsm-gateway
endif
# Deposit addresses are issued from the Attested xpub, verified on every issuance against the attestation public key pinned in a file outside the
# database: $(HSM_DIR)/attest-pubkey, written by hsm-setup on the gateway node (copy it to vm3 next to the wallet-service leaf).
export XPUB_PIN_HOST = $(shell if [ -f $(HSM_DIR)/attest-pubkey ]; then echo $(HSM_DIR)/attest-pubkey; fi)
export XPUB_ATTEST_PUBKEY_FILE := /config/attest-pubkey
# sweep-signer's and key-admin's host files (database passwords as FILES, the pinned sweeper leaf) live here, outside the repo's tracked files.
# sweep-signer learns the Hot address from hsm-gateway and gets the seed by gateway unseal.
export SWEEP_DIR ?= $(abspath sweep-config)
export SWEEP_HOT_SOURCE := gateway
# The ceremony job (key-admin: no key of its own; talks to hsm-gateway as CN key-admin and to the vault as svc_keyadmin).
KA = XPUB_PIN_HOST=$(HSM_DIR)/attest-pubkey COMPOSE_PROFILES=keyadmin $(COMPOSE) run --rm -T key-admin
ifneq ($(NODE),)
FILES += -f nodes/$(NODE).yml
# On a node the host is root and so is the owner of policy.yaml / callers.yaml: hsm-gateway then REFUSES a policy file that is not root-owned
# (key-custody-hsm.md 5.1 condition 3). A single host (a developer's laptop) is not root, so the check stays off there.
export HSM_POLICY_REQUIRE_ROOT_OWNER ?= true
endif
COMPOSE = docker compose -p digiwal-dev --project-directory ../e2e --env-file .env $(FILES)

ifneq ($(filter $(NODE),vm2 vm4),)
ifeq ($(NODE),vm2)
STORES   = postgres mongo
SERVICES =
MONITORING = node-exporter cadvisor postgres-exporter
else
STORES   =
SERVICES = ledger-service history-service withdrawal-service
MONITORING = node-exporter cadvisor
endif
else ifeq ($(NODE),vm3)
STORES   = postgres postgres-gateway redis kafka anvil
SERVICES = wallet-service chain-watcher sweeper sweep-signer api-gateway simulator
MONITORING = node-exporter cadvisor postgres-exporter kafka-exporter alertmanager prometheus grafana
else
STORES   = postgres redis kafka mongo anvil
SERVICES = wallet-service ledger-service chain-watcher sweeper sweep-signer api-gateway withdrawal-service history-service simulator
MONITORING =
endif
TOPICS = deposits.detected deposits.confirmed withdrawals.broadcast withdrawals.confirmed withdrawals.failed

.PHONY: up down reset status logs sim-logs users certs certs-issue push-certs env roles audit-roles monitoring firewall verify hsm-setup hsm-check \
	hot-cutover-preflight hot-cutover-freeze hot-cutover-settle hot-cutover-drain hot-cutover-switch hot-cutover-restart \
	hot-cutover-verify hot-cutover-complete hot-cutover-abort init vault-files vault-roles vault-role-check start

# Secrets are generated once and kept in the gitignored .env, per node. There is no master key: the user_derivation seed is sealed under a
# KEK that never leaves the HSM token on vm2 (docs/runbooks/dev-hsm-bringup.md). REDIS_PASSWORD is generated on vm3 (it hosts Redis); vm4
# (api-gateway) copies it from there.
env:
	@touch .env; chmod 600 .env
	@gen() { grep -q "^$$1=" .env || { echo "$$1=$$(openssl rand -hex 32)" >> .env; echo "generated $$1"; }; }; \
	case "$(NODE)" in \
	vm2) for v in PG_ADMIN_PASSWORD SVC_LEDGER_PASSWORD SVC_GATEWAY_PASSWORD SVC_HISTORY_PASSWORD MONGO_ADMIN_PASSWORD EXPORTER_PASSWORD; do gen $$v; done ;; \
	vm4) gen PG_ADMIN_PASSWORD; for v in SVC_LEDGER_PASSWORD SVC_GATEWAY_PASSWORD SVC_HISTORY_PASSWORD SVC_WITHDRAWAL_PASSWORD; do grep -q "^$$v=" .env || { echo "$$v missing: copy it from vm2's .env (the databases' passwords are set there)"; exit 1; }; done ;; \
	vm3) for v in PG_ADMIN_PASSWORD SVC_WALLET_PASSWORD SVC_WITHDRAWAL_PASSWORD SVC_CHAINWATCHER_PASSWORD SVC_AUDITOR_PASSWORD EXPORTER_PASSWORD SVC_SWEEPSIGNER_PASSWORD SVC_KEYADMIN_PASSWORD REDIS_PASSWORD; do gen $$v; done; \
		grep -q '^SVC_GATEWAY_PASSWORD=' .env || { pw=$$(ssh vm4 "sed -n 's/^SVC_GATEWAY_PASSWORD=//p' /root/DigiWal/dev/.env" 2>/dev/null); \
		echo "$$pw" | grep -Eq '^[0-9a-f]{64}$$' || { echo "SVC_GATEWAY_PASSWORD: vm4 has none (it is the password of the api_gateway role in postgres-gateway; api-gateway on vm3 copies it from there)"; exit 1; }; \
		echo "SVC_GATEWAY_PASSWORD=$$pw" >> .env; echo "copied SVC_GATEWAY_PASSWORD from vm4"; } ;; \
	*) gen SVC_SWEEPSIGNER_PASSWORD; gen SVC_KEYADMIN_PASSWORD ;; \
	esac

# The CA key lives on the OPERATOR machine (CA_DIR there), never on a node that runs workloads (key-custody-hsm.md 5.1 condition 2;
# runbook: ../docs/runbooks/ca-key-off-vm2.md). Leaves are issued on the operator machine and pushed per node, key and all (over ssh), each owned
# by its service's fixed uid with the key 0400 (infra/pki/leaf-uid.sh); a node never signs anything. On a single host (no /etc/digiwal/node) the CA is
# local, as in ../e2e. The node a service runs on, and the callers whose PUBLIC certificate hsm-gateway pins (vm2 gets their fingerprints, not keys):
export CA_DIR ?= $(HOME)/.digiwal-ca
LEAVES_vm2 = hsm-gateway
LEAVES_vm3 = wallet-service chain-watcher sweeper sweep-signer api-gateway hot-cutover key-admin auditor
LEAVES_vm4 = ledger-service withdrawal-service
PINNED_ON_VM2 = wallet-service sweep-signer key-admin
REMOTE_DIR ?= /root/DigiWal
VM2 ?= vm2
VM3 ?= vm3
VM4 ?= vm4

certs:
ifneq ($(NODE),)
	@for s in $(LEAVES_$(NODE)); do test -f ../infra/pki/out/$$s/cert.pem || { echo "missing leaf for $$s: run 'make push-certs' on the operator machine (the CA key is not on a node)"; exit 1; }; done
	@test ! -f "$(CA_DIR)/ca-key.pem" || echo "WARNING: a CA key is on this node ($(CA_DIR)/ca-key.pem): move it to the operator machine (docs/runbooks/ca-key-off-vm2.md); make verify fails while it is here"
else
	$(MAKE) -C ../e2e certs
	@test -f ../infra/pki/out/auditor/cert.pem || ../infra/pki/gen-leaf.sh auditor
	@chmod 700 ../infra/pki/out
endif

# OPERATOR MACHINE ONLY (CA_DIR holds the CA key; created on first use). Issues every leaf that is missing, then tightens permissions.
certs-issue:
	@test -z "$(NODE)" || { echo "run on the operator machine, not on a node: a node must never hold the CA key"; exit 1; }
	@test -f "$(CA_DIR)/ca-key.pem" || ../infra/pki/gen-ca.sh
	@for s in $(LEAVES_vm2) $(LEAVES_vm3) $(LEAVES_vm4); do test -f ../infra/pki/out/$$s/cert.pem || ../infra/pki/gen-leaf.sh $$s || exit 1; done
	@chmod 700 ../infra/pki/out; ../infra/pki/fix-perms.sh $(addprefix ../infra/pki/out/,$(LEAVES_vm2) $(LEAVES_vm3) $(LEAVES_vm4))

# OPERATOR MACHINE ONLY: each node gets its own leaves (key included) and, on vm2, the public certificates hsm-gateway pins. The node chowns them to
# the service uid (fix-perms.sh, as root there). Re-run after rotating a leaf, then `make hsm-setup` on vm2 (re-pins) and restart the gateway.
push-certs: certs-issue
	@set -e; push() { host=$$1; svc=$$2; full=$$3; d=$(REMOTE_DIR)/infra/pki/out/$$svc; \
		ssh $$host "mkdir -p $(REMOTE_DIR)/infra/pki/out && chmod 700 $(REMOTE_DIR)/infra/pki/out && mkdir -p $$d && chmod u+w $$d"; \
		if $$full; then rsync -rt --delete -e ssh ../infra/pki/out/$$svc/ $$host:$$d/; ssh $$host "cd $(REMOTE_DIR) && infra/pki/fix-perms.sh infra/pki/out/$$svc"; \
		else rsync -rt --delete -e ssh --exclude key.pem ../infra/pki/out/$$svc/ $$host:$$d/; ssh $$host "chmod 755 $$d && chmod 644 $$d/*.pem"; fi; }; \
		for s in $(LEAVES_vm2); do push $(VM2) $$s true; done; for s in $(PINNED_ON_VM2); do push $(VM2) $$s false; done; \
		for s in $(LEAVES_vm3); do push $(VM3) $$s true; done; for s in $(LEAVES_vm4); do push $(VM4) $$s true; done; \
		echo "pushed: vm2 ($(LEAVES_vm2), public: $(PINNED_ON_VM2)), vm3 ($(LEAVES_vm3)), vm4 ($(LEAVES_vm4))"

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
	$(MAKE) init
endif
	$(MAKE) hsm-check
	$(MAKE) start
ifneq ($(NODE),)
	$(MAKE) monitoring
endif

# A separate make process on purpose: XPUB_PIN_HOST depends on whether the pin file exists (an exported
# variable is expanded once per make process).
start:
	$(COMPOSE) up -d --build --wait $(HSM_SERVICES) $(SERVICES)

# The vault bring-up of this node (vm3, or the single host), idempotent: a second run finds the active row and changes nothing. Runbook:
# ../docs/runbooks/dev-hsm-bringup.md. The HSM ceremony (`make hsm-setup`, vm2 or the single host) and the pin file come first.
#   1. host files: database passwords as FILES from .env, the pinned sweeper leaf
#   2. custody migrations (key-admin migrate), then the two least-privilege roles and the vault schema (psql as the Postgres admin)
#   3. key-admin init: generate the seed, have hsm-gateway seal it (KEK in the HSM), attest the xpub, prove address parity, activate
init: vault-files
	@test "$(NODE)" != vm2 -a "$(NODE)" != vm4 || { echo "run on vm3 (or a single host): the vault lives in custody's database"; exit 1; }
	@test -f $(HSM_DIR)/attest-pubkey || { echo "missing $(HSM_DIR)/attest-pubkey: the attestation pin from the HSM ceremony (make hsm-setup on the gateway node, then copy it here)"; exit 1; }
	@test -f ../infra/pki/out/key-admin/cert.pem || { echo "missing leaf for key-admin: run 'make push-certs' on the operator machine"; exit 1; }
	COMPOSE_PROFILES=keyadmin $(COMPOSE) build -q key-admin
	$(KA) migrate
	$(MAKE) vault-roles
	@$(KA) status; rc=$$?; \
	if [ $$rc -eq 0 ]; then echo "already initialised: nothing to do"; \
	elif [ $$rc -eq 3 ]; then echo "initialising: WRITE DOWN THE MNEMONIC BELOW (shown once)"; $(KA) init || exit 1; \
	else echo "key-admin status failed (exit $$rc)"; exit 1; fi

# The host files: the database passwords of svc_sweepsigner and svc_keyadmin as FILES from this node's .env, each mode 0400 and owned by the uid of the
# container that reads it (sweep-signer 10002, key-admin 10004; the chown needs root, which a node is), and the pinned sweeper leaf. Re-run after
# rotating the sweeper leaf (the pin changes).
vault-files:
	@mkdir -p $(SWEEP_DIR) && chmod 755 $(SWEEP_DIR)
	@. ./.env && put() { rm -f "$(SWEEP_DIR)/$$1"; (umask 077; printf '%s\n' "$$2" > "$(SWEEP_DIR)/$$1"); chmod 400 "$(SWEEP_DIR)/$$1"; \
		[ "$$(id -u)" != 0 ] || chown "$$3:$$3" "$(SWEEP_DIR)/$$1"; }; \
		put db-password "$$SVC_SWEEPSIGNER_PASSWORD" 10002; put keyadmin-db-password "$$SVC_KEYADMIN_PASSWORD" 10004
	@test -f ../infra/pki/out/sweeper/cert.pem || { echo "missing leaf for sweeper"; exit 1; }
	../infra/pki/gen-callers.sh $(SWEEP_DIR)/callers.yaml sweeper

# The roles and the vault schema, as the Postgres admin (after the migrations), and the proofs: svc_sweepsigner reads four columns, the xpub and the
# ACTIVE vault row; svc_keyadmin writes the vault through a guard trigger; svc_wallet (wallet-service) has NO access to the vault.
vault-roles:
	@. ./.env && $(COMPOSE) exec -T postgres psql -U custody -d custody -v ON_ERROR_STOP=1 -q -v pw="$$SVC_SWEEPSIGNER_PASSWORD" -f - < ../infra/postgres/sweep-signer-role.sql
	@. ./.env && $(COMPOSE) exec -T postgres psql -U custody -d custody -v ON_ERROR_STOP=1 -q -v pw_keyadmin="$$SVC_KEYADMIN_PASSWORD" -f - < ../infra/postgres/vault.sql
	$(MAKE) vault-role-check

vault-role-check:
	$(COMPOSE) exec -T postgres psql -U custody -d custody -v ON_ERROR_STOP=1 -q -f - < ../infra/postgres/sweep-signer-role-check.sql
	$(COMPOSE) exec -T postgres psql -U custody -d custody -v ON_ERROR_STOP=1 -q -v walletrole=svc_wallet -f - < ../infra/postgres/vault-role-check.sql

# Key rotation (docs/runbooks/dev-hsm-bringup.md, "Rotation"): open a Maintenance Window on the gateway node first (vm2 or the single host):
#   make maintenance-open     then on vm3:   make key-admin CMD=rotate-kek   (rewrap | activate | rotate-dek | rollback | retire-kek N | destroy-kek N | status)
.PHONY: maintenance-open maintenance-close key-admin
maintenance-open:
	@test "$(NODE)" != vm3 -a "$(NODE)" != vm4 || { echo "run on the gateway node (vm2 or a single host)"; exit 1; }
	$(COMPOSE) run --rm -T hsm-gateway maintenance open 15m
maintenance-close:
	$(COMPOSE) run --rm -T hsm-gateway maintenance close
key-admin:
	@test -n "$(CMD)" || { echo "make key-admin CMD='status|rotate-kek|rotate-dek|rewrap|activate|rollback|retire-kek N|destroy-kek N'"; exit 1; }
	$(KA) $(CMD)

# pkcs11 only: refuse to start the gateway (and, on vm2, anything else) before the ceremony has been done, instead of
# letting Docker create empty directories where policy.yaml should be.
hsm-check:
ifeq ($(filter $(NODE),vm3 vm4),)
	@test -f $(HSM_DIR)/hot-address -a -f $(HSM_DIR)/policy.yaml -a -f $(HSM_DIR)/callers.yaml || { echo "run 'make hsm-setup' first (token ceremony: Hot Key, attestation key, KEK)"; exit 1; }
endif

# The HSM ceremony (vm2 or single host; once per environment): leaf for CN hsm-gateway, PIN, the pinned wallet-service leaf
# (callers.yaml), token + Hot Key generated INSIDE SoftHSM, then the Signing Policy with the printed Hot address. The SO PIN
# is random and discarded (dev only; production types it once, see services/hsm-gateway/README.md). Re-running is a no-op
# once hot-address exists; re-issued leaves are re-pinned (restart the gateway after).
hsm-setup: certs
	@test "$(NODE)" != vm3 -a "$(NODE)" != vm4 || { echo "run on vm2 (or a single host): the gateway lives there"; exit 1; }
	@for s in hsm-gateway $(PINNED_ON_VM2); do test -f ../infra/pki/out/$$s/cert.pem || { echo "missing leaf (or public certificate) for $$s: run 'make push-certs' on the operator machine"; exit 1; }; done
	$(COMPOSE) build -q hsm-perms
	../infra/hsm/prepare.sh $(HSM_DIR)
	@if [ -f $(HSM_DIR)/hot-address ]; then echo "token already initialised: hot address $$(cat $(HSM_DIR)/hot-address)"; \
		if [ ! -f $(HSM_DIR)/attest-pubkey ]; then echo "token from before M3a: generating its attestation key (init-attest-key)"; \
			out=$$($(COMPOSE) run --rm -T hsm-gateway init-attest-key) || exit 1; \
			printf '%s\n' "$$out" | ../infra/hsm/pin-attest-key.sh $(HSM_DIR) || exit 1; fi; \
		if ! $(COMPOSE) run --rm -T hsm-gateway init-kek >/dev/null 2>&1; then echo "KEK present (init-kek refused: kek-v1 exists)"; else echo "token from before M3c: generated its KEK (init-kek); restart hsm-gateway"; fi; \
	else \
		test -f $(HSM_DIR)/policy.yaml || ../infra/hsm/write-policy.sh $(HSM_DIR) 0x0000000000000000000000000000000000000001; \
		out=$$(openssl rand -hex 12 | $(COMPOSE) run --rm -T hsm-gateway init-token) || exit 1; \
		addr=$$(printf '%s\n' "$$out" | sed -n 's/^hot_address=//p'); \
		test -n "$$addr" || { echo "init-token printed no address"; exit 1; }; \
		printf '%s\n' "$$out" | grep -q '^kek_version=1' || { echo "init-token generated no KEK"; exit 1; }; \
		printf '%s\n' "$$out" | ../infra/hsm/pin-attest-key.sh $(HSM_DIR) || exit 1; \
		../infra/hsm/write-policy.sh $(HSM_DIR) $$addr && echo "$$addr" > $(HSM_DIR)/hot-address; fi

# ---------------------------------------------------------------------------------------------------------------------
# The Hot cutover of an environment that already runs on the software Hot (key-custody-hsm.md 5.5, M2c).
# Runbook: ../docs/runbooks/hot-cutover.md. The tool runs on the node that has custody's database and wallet-service's
# leaf (vm3, or the single host); the ceremony (`make hsm-setup SIGNER_BACKEND=pkcs11`) and `make up SIGNER_BACKEND=pkcs11` on
# vm2 come first. DRY_RUN=1 prints what a step would do and changes nothing. NEW_HOT is the address the ceremony printed
# (vm2: $(HSM_DIR)/hot-address, pass it on vm3); the tool refuses if hsm-gateway's key is another one.
# ---------------------------------------------------------------------------------------------------------------------
CONFIRMATIONS ?= 2
NEW_HOT ?= $(shell cat $(HSM_DIR)/hot-address 2>/dev/null)
CUTOVER = COMPOSE_PROFILES=cutover $(COMPOSE) run --rm -T hot-cutover $(if $(DRY_RUN),-dry-run) -confirmations $(CONFIRMATIONS) -new $(NEW_HOT)

# Every step rebuilds the tool image first (cached, a second): an image left from an earlier checkout would run old code.
.PHONY: cutover-guard hot-cutover-image
hot-cutover-image:
	COMPOSE_PROFILES=cutover $(COMPOSE) build -q hot-cutover

cutover-guard: hot-cutover-image
	@test "$(NODE)" != vm2 -a "$(NODE)" != vm4 || { echo "run on vm3 (or a single host): the tool needs custody's database"; exit 1; }
	@test -f ../infra/pki/out/hot-cutover/cert.pem || { echo "missing leaf for hot-cutover: run 'make push-certs' on the operator machine"; exit 1; }
	@test -n "$(NEW_HOT)" || { echo "NEW_HOT=0x... (the address the HSM ceremony printed)"; exit 1; }

hot-cutover-preflight hot-cutover-settle hot-cutover-drain: cutover-guard
	$(CUTOVER) $(patsubst hot-cutover-%,%,$@)

# Step 2: refuse early (preflight), stop the sweeper, freeze new Hot signing.
hot-cutover-freeze: cutover-guard
	$(CUTOVER) preflight
	$(if $(DRY_RUN),,$(COMPOSE) stop sweeper)
	$(CUTOVER) freeze

# Steps 8-9: wallet-service serves the HSM key from now on, then its consumers re-read the address (cached at start).
# The sweeper stays stopped until hot-cutover-complete has unfrozen signing. (The dev auditor is not part of this stack.)
hot-cutover-switch:
	$(MAKE) SIGNER_BACKEND=pkcs11 hot-cutover-restart

hot-cutover-restart:
	@test "$(SIGNER_BACKEND)" = pkcs11 || { echo "use: make hot-cutover-switch"; exit 1; }
	$(COMPOSE) up -d --no-deps --force-recreate --wait wallet-service
	@# sweep-signer pins the Hot address once, at start: recreate it so it pins the new one, from hsm-gateway (SIGNER_BACKEND=pkcs11 now)
	$(COMPOSE) up -d --no-deps --force-recreate --wait sweep-signer
	$(COMPOSE) restart chain-watcher
	$(if $(filter vm3,$(NODE)),@echo "NOW, ON vm4: make -C dev hot-cutover-restart-withdrawal  (withdrawal-service caches the Hot address; it runs on vm4)",$(COMPOSE) restart withdrawal-service)

# vm4 (three-node layout): withdrawal-service caches the Hot address at start, so it is restarted there after the switch.
.PHONY: hot-cutover-restart-withdrawal
hot-cutover-restart-withdrawal:
	@test "$(NODE)" = vm4 || { echo "run on vm4: withdrawal-service runs there"; exit 1; }
	$(COMPOSE) restart withdrawal-service

hot-cutover-verify: cutover-guard
	$(CUTOVER) verify
	@$(COMPOSE) logs wallet-service 2>&1 | grep -qi 'Hot Key: hsm-gateway at .*address $(NEW_HOT)' \
		|| { echo "FAIL: wallet-service's log has no 'Hot Key: hsm-gateway ... address $(NEW_HOT)' line"; exit 1; }
	@echo "wallet-service pinned $(NEW_HOT) from hsm-gateway (the hot_wallet seed is not loaded)"

# Step 9 (after the restarts): unfreeze, then start the sweeper (a fresh process reads the new Hot address).
hot-cutover-complete: cutover-guard
	$(CUTOVER) complete -switched
	$(if $(DRY_RUN),,$(COMPOSE) up -d --no-deps sweeper)

# Before the point of no return only.
hot-cutover-abort: cutover-guard
	$(CUTOVER) abort
	$(if $(DRY_RUN),,$(COMPOSE) up -d --no-deps sweeper)

down:
	$(COMPOSE) down --remove-orphans

# (the hsm profile is on for the down so the gateway's token/audit volumes go too; the ceremony files would be stale)
reset:
ifneq ($(NODE),vm3)
	COMPOSE_PROFILES=hsm $(COMPOSE) down -v --remove-orphans
	rm -rf $(HSM_DIR) $(SWEEP_DIR)
else
	$(COMPOSE) down -v --remove-orphans
	rm -rf $(SWEEP_DIR)
endif

status:
	$(COMPOSE) ps --format 'table {{.Service}}\t{{.Status}}\t{{.Ports}}'

logs:
	$(COMPOSE) logs -f --tail=100 $(SVC)

sim-logs:
	$(COMPOSE) logs -f --tail=100 simulator

users:
	$(COMPOSE) exec simulator sim user list

verify:
	@NODE=$(NODE) SIGNER_BACKEND=$(SIGNER_BACKEND) ./verify.sh
