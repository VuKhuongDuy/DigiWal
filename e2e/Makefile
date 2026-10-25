# e2e for Milestones 2-3 (deposit -> ledger balance -> sweep) against a local Anvil chain.
#   make e2e        bring the stack up, run the tests, tear it down (volumes included)       Hot Wallet = SOFTWARE key (regression baseline)
#   make e2e-hsm    the same with SIGNER_BACKEND=pkcs11: the Hot Wallet is signed by hsm-gateway
#   make up / test / down / logs   the same steps one at a time, for debugging
# In BOTH modes the user_derivation seed lives in the vault: hsm-gateway runs (SoftHSM token with a KEK), `key-admin init` seals the seed through it,
# and sweep-signer gets the seed by gateway unseal, per Sweep (key-custody-hsm.md M3c). Runbook: ../docs/runbooks/dev-hsm-bringup.md.
COMPOSE = docker compose -p digiwal-e2e --project-directory . -f ../infra/docker-compose.yml -f docker-compose.yml -f docker-compose.hsm.yml -f docker-compose.auditor.yml
SERVICES = wallet-service ledger-service chain-watcher sweeper sweep-signer api-gateway withdrawal-service

# SIGNER_BACKEND=pkcs11 (`make e2e-hsm`): the Hot Wallet is signed by a real hsm-gateway with a real SoftHSMv2 token. Unset = software.
export SIGNER_BACKEND ?= software
export HSM_DIR ?= $(abspath hsm-config)
# sweep-signer's and key-admin's host files: database passwords as FILES, the pinned sweeper leaf.
export SWEEP_DIR ?= $(abspath sweep-config)
# The gateway runs in every mode: it holds the KEK that seals the user_derivation seed (and, with pkcs11, the Hot Key).
export COMPOSE_PROFILES := hsm
ifeq ($(SIGNER_BACKEND),pkcs11)
# sweep-signer pins the Hot address from hsm-gateway; wallet-service has no software key at all.
export SWEEP_HOT_SOURCE := gateway
else
# LEGACY dev profile: the Hot Wallet is a software key (a !prod build) sealed under the OLD software key below, and sweep-signer, which then
# has no gateway Hot key to ask, reads the Hot address from wallet-service (a DEV-only source, compiled out of a prod image). This is the only
# thing in this directory that still uses MASTER_KEY_HEX; the vault does not.
export WALLET_SOFTWARE_HOT := 1
export MASTER_KEY_HEX ?= 0000000000000000000000000000000000000000000000000000000000000001
export SWEEP_HOT_SOURCE := wallet-service
endif
# Deposit addresses come from the Attested xpub: wallet-service verifies it on every issuance against the attestation public key pinned in a file
# outside the database: $(HSM_DIR)/attest-pubkey, written by hsm-setup from the token's own attestation key.
export XPUB_PIN_HOST = $(HSM_DIR)/attest-pubkey
export XPUB_ATTEST_PUBKEY_FILE := /config/attest-pubkey
# The ceremony job (key-admin: no key of its own; talks to hsm-gateway as CN key-admin).
KA = XPUB_PIN_HOST=$(HSM_DIR)/attest-pubkey COMPOSE_PROFILES=keyadmin,hsm $(COMPOSE) run --rm -T key-admin

.PHONY: e2e e2e-hsm e2e-cutover certs up start-services test down logs hsm-setup init xpub-check vault-files vault-roles vault-role-check sweep-role-check

e2e:
	$(MAKE) up
	$(MAKE) test; rc=$$?; $(MAKE) down; exit $$rc

e2e-hsm:
	$(MAKE) SIGNER_BACKEND=pkcs11 up
	$(MAKE) SIGNER_BACKEND=pkcs11 test; rc=$$?; \
		if [ $$rc -eq 0 ]; then $(MAKE) SIGNER_BACKEND=pkcs11 xpub-check; rc=$$?; fi; \
		$(MAKE) SIGNER_BACKEND=pkcs11 down; exit $$rc

certs:
	@test -f "$${CA_DIR:-$$HOME/.digiwal-ca}/ca-key.pem" || ../infra/pki/gen-ca.sh
	@for svc in $(SERVICES) key-admin hsm-gateway; do test -f ../infra/pki/out/$$svc/cert.pem || ../infra/pki/gen-leaf.sh $$svc; done
	@# leaves issued before the 0400/0500 rule are tightened in place (key-custody-hsm.md invariant 27; a no-op for new ones)
	@../infra/pki/fix-perms.sh $(addprefix ../infra/pki/out/,$(SERVICES) key-admin hsm-gateway)

# The HSM ceremony for this throwaway stack (idempotent: a no-op once $(HSM_DIR)/hot-address exists): PIN, pinned caller leaves, token with the Hot Key,
# the attestation key and the KEK generated inside SoftHSM (the SO PIN is random and discarded: this token is dropped with the stack), the
# Signing Policy with the printed Hot address, and the attestation public key pinned into $(HSM_DIR)/attest-pubkey (outside the database).
hsm-setup:
	$(COMPOSE) build -q hsm-perms
	../infra/hsm/prepare.sh $(HSM_DIR)
	@if [ -f $(HSM_DIR)/hot-address ]; then echo "token already initialised: hot address $$(cat $(HSM_DIR)/hot-address)"; exit 0; fi; \
	test -f $(HSM_DIR)/policy.yaml || ../infra/hsm/write-policy.sh $(HSM_DIR) 0x0000000000000000000000000000000000000001; \
	out=$$(openssl rand -hex 12 | $(COMPOSE) run --rm -T hsm-gateway init-token) || exit 1; \
	addr=$$(printf '%s\n' "$$out" | sed -n 's/^hot_address=//p'); \
	test -n "$$addr" || { echo "init-token printed no address"; exit 1; }; \
	printf '%s\n' "$$out" | grep -q '^kek_version=1' || { echo "init-token generated no KEK"; exit 1; }; \
	printf '%s\n' "$$out" | ../infra/hsm/pin-attest-key.sh $(HSM_DIR) || exit 1; \
	../infra/hsm/write-policy.sh $(HSM_DIR) $$addr && echo "$$addr" > $(HSM_DIR)/hot-address

# The tests deploy the ERC-20 first thing, so a fresh chain matters: `down` always drops it.
up: certs
	$(COMPOSE) up -d --wait postgres redis kafka mongo anvil
	# Topics must exist before any consumer starts: kafka-go's group reader that joins while its
	# topic is missing never gets the partitions afterwards (found by this e2e — ledger-service and
	# history-service sat idle until restarted). Names from pkg/events.
	@for t in deposits.detected deposits.confirmed withdrawals.broadcast withdrawals.confirmed withdrawals.failed; do \
		$(COMPOSE) exec -T kafka /opt/kafka/bin/kafka-topics.sh --bootstrap-server localhost:9092 --create --if-not-exists --topic $$t --partitions 1 --replication-factor 1 || exit 1; done
	$(MAKE) hsm-setup
	$(COMPOSE) up -d --build hsm-gateway
	$(MAKE) init
	$(MAKE) start-services

# A separate make process on purpose (an exported variable is expanded once per make process, and init has just written the pin).
start-services:
	$(COMPOSE) up -d --build --wait $(SERVICES) history-service
	@$(COMPOSE) logs wallet-service 2>&1 | grep -q 'deposit addresses issue from the attested xpub' \
		|| { echo "FAIL: wallet-service is not issuing from the attested xpub (see $(COMPOSE) logs wallet-service)"; exit 1; }
	@echo "wallet-service issues deposit addresses from the attested xpub"
	@$(COMPOSE) logs sweep-signer 2>&1 | grep -q 'Hot address pinned from' \
		|| { echo "FAIL: sweep-signer did not pin the Hot address (see $(COMPOSE) logs sweep-signer)"; exit 1; }
	@echo "sweep-signer is up: $$($(COMPOSE) logs sweep-signer 2>&1 | grep -m1 'Hot address pinned from' | sed 's/^.*Hot address pinned from/Hot address pinned from/' | cut -c1-110)"

# The vault bring-up of this stack (idempotent; runbook ../docs/runbooks/dev-hsm-bringup.md). After the HSM ceremony: the host files, the custody
# migrations, the two least-privilege roles with their grants and proofs, then `key-admin init` (generate the seed, seal it through the gateway,
# derive and attest the xpub, prove address parity, flip the row active). A second run finds the active row and does nothing.
init: vault-files
	COMPOSE_PROFILES=keyadmin,hsm $(COMPOSE) build -q key-admin
	$(KA) migrate
ifneq ($(SIGNER_BACKEND),pkcs11)
	@# LEGACY software Hot Wallet seed (regression baseline only; a !prod wallet-service build; sealed under the old software key)
	$(COMPOSE) build -q wallet-service
	@q() { $(COMPOSE) exec -T postgres psql -U custody -d custody -tAc "$$1" 2>/dev/null || echo 0; }; \
	if [ "$$(q "SELECT count(*) FROM wallet_seeds WHERE seed_role='hot_wallet'")" -lt 1 ]; then \
		XPUB_PIN_HOST=/dev/null $(COMPOSE) run --rm wallet-service -init-seed hot_wallet >/dev/null || exit 1; fi
endif
	$(MAKE) vault-roles
	@$(KA) status; rc=$$?; \
	if [ $$rc -eq 0 ]; then echo "already initialised: nothing to do"; \
	elif [ $$rc -eq 3 ]; then $(KA) init >/dev/null && echo "key-admin init: done (mnemonic discarded: throwaway stack)" || exit 1; \
	else echo "key-admin status failed (exit $$rc)"; exit 1; fi

# sweep-signer's and key-admin's host files: the database passwords as FILES, mode 0400 and owned by the reading container's uid (10002, 10004; the chown
# happens only as root: Docker Desktop maps the owner of a bind mount, so a developer's own files still read), and the pinned sweeper leaf.
vault-files:
	@mkdir -p $(SWEEP_DIR) && chmod 755 $(SWEEP_DIR)
	@put() { test -f "$(SWEEP_DIR)/$$1" || (umask 077; openssl rand -hex 16 > "$(SWEEP_DIR)/$$1"); chmod 400 "$(SWEEP_DIR)/$$1"; \
		[ "$$(id -u)" != 0 ] || chown "$$2:$$2" "$(SWEEP_DIR)/$$1"; }; put db-password 10002; put keyadmin-db-password 10004
	../infra/pki/gen-callers.sh $(SWEEP_DIR)/callers.yaml sweeper

# The database roles (run after the migrations): svc_sweepsigner (read-only on four columns, the xpub and the ACTIVE vault row), svc_keyadmin (writes the
# vault through a guard trigger), the vault schema nobody else can touch, and the proofs. psql runs as the stack's superuser.
vault-roles:
	@$(COMPOSE) exec -T postgres psql -U custody -d custody -v ON_ERROR_STOP=1 -q -v pw="$$(cat $(SWEEP_DIR)/db-password)" -f - < ../infra/postgres/sweep-signer-role.sql
	@$(COMPOSE) exec -T postgres psql -U custody -d custody -v ON_ERROR_STOP=1 -q -v pw_keyadmin="$$(cat $(SWEEP_DIR)/keyadmin-db-password)" -f - < ../infra/postgres/vault.sql
	$(MAKE) vault-role-check

# wallet-service of this stack connects as the superuser (the e2e Postgres has one login), which cannot be denied anything: the proof that
# wallet-service's role is locked out uses a stand-in with the grants of a database-owner login (dev/ proves it with the real svc_wallet).
vault-role-check:
	$(COMPOSE) exec -T postgres psql -U custody -d custody -v ON_ERROR_STOP=1 -q -f - < ../infra/postgres/sweep-signer-role-check.sql
	@$(COMPOSE) exec -T postgres psql -U custody -d custody -q -c "DO \$$\$$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='e2e_wallet_probe') THEN CREATE ROLE e2e_wallet_probe NOLOGIN; END IF; END \$$\$$; GRANT ALL ON ALL TABLES IN SCHEMA public TO e2e_wallet_probe; GRANT USAGE, CREATE ON SCHEMA public TO e2e_wallet_probe;"
	$(COMPOSE) exec -T postgres psql -U custody -d custody -v ON_ERROR_STOP=1 -q -v walletrole=e2e_wallet_probe -f - < ../infra/postgres/vault-role-check.sql

sweep-role-check: vault-role-check

# After the tests: the deposit addresses of this run really came from the xpub (its counter moved) and the seed is in the vault (one active row).
xpub-check:
	@x=$$($(COMPOSE) exec -T postgres psql -U custody -d custody -tAc "SELECT next_index FROM wallet_xpubs WHERE chain='evm'"); \
	v=$$($(COMPOSE) exec -T postgres psql -U custody -d custody -tAc "SELECT count(*) FROM vault.sealed_secrets WHERE role='user_derivation' AND status='active'"); \
	a=$$($(COMPOSE) exec -T postgres psql -U custody -d custody -tAc "SELECT count(*) FROM wallet_addresses"); \
	echo "wallet_xpubs.next_index=$$x vault_active_rows=$$v wallet_addresses=$$a"; \
	test "$${x:-0}" -ge 1 -a "$${v:-0}" -eq 1 -a "$$a" -eq "$$x" || { echo "FAIL: issuance did not go through the xpub, or the vault has no single active row"; exit 1; }; \
	echo "OK: every deposit address of this run was issued from the attested xpub; the seed is sealed in the vault"

test:
	E2E=1 go test -count=1 -v -timeout 10m ./...

down:
	$(COMPOSE) down -v --remove-orphans
	rm -rf $(HSM_DIR) $(SWEEP_DIR)

logs:
	$(COMPOSE) logs -f

# ---------------------------------------------------------------------------------------------------------------------
# The Hot cutover (key-custody-hsm.md 5.5, M2c; runbook: ../docs/runbooks/hot-cutover.md).
#
#   make e2e-cutover            the LOCAL REHEARSAL: software Hot with funds, a stuck tx and an in-flight Gas Top-Up,
#                               an HSM key brought up, the cutover run, then everything proved. Tears the stack down.
#   make hot-cutover-<step>     one step of the procedure against the running stack (DRY_RUN=1 prints, changes nothing):
#                               preflight | freeze | settle | drain | switch | verify | complete | abort
# NEW_HOT is the address the HSM ceremony printed (hsm-config/hot-address here); the tool refuses if hsm-gateway's key differs.
# ---------------------------------------------------------------------------------------------------------------------
CONFIRMATIONS ?= 2
NEW_HOT ?= $(shell cat $(HSM_DIR)/hot-address 2>/dev/null)
CUTOVER = $(COMPOSE) run --rm -T hot-cutover $(if $(DRY_RUN),-dry-run) -confirmations $(CONFIRMATIONS) -new $(NEW_HOT)
ANVIL = http://localhost:8545
rpc = curl -sf -X POST -H 'content-type: application/json' --data '{"jsonrpc":"2.0","id":1,"method":"$(1)","params":$(2)}' $(ANVIL) >/dev/null

.PHONY: hot-cutover-preflight hot-cutover-freeze hot-cutover-settle hot-cutover-drain hot-cutover-switch hot-cutover-restart \
	hot-cutover-verify hot-cutover-complete hot-cutover-abort cutover-certs cutover-up cutover-down cutover-hsm \
	cutover-auditor-roles cutover-audit cutover-audit-baseline cutover-test cutover-rehearse

# Every step rebuilds the tool image first (cached, a second): an image left from an earlier checkout would run old code.
.PHONY: hot-cutover-image
hot-cutover-image:
	$(COMPOSE) build -q hot-cutover

hot-cutover-preflight hot-cutover-settle hot-cutover-drain: hot-cutover-image
	$(CUTOVER) $(patsubst hot-cutover-%,%,$@)

# Step 2: refuse early (preflight), stop the sweeper, freeze new Hot signing.
hot-cutover-freeze: hot-cutover-image
	$(CUTOVER) preflight
	$(if $(DRY_RUN),,$(COMPOSE) stop sweeper)
	$(CUTOVER) freeze

# Steps 8-9: wallet-service now serves the HSM key, then its consumers read the new address (they cache it at start).
# The sweeper stays stopped until hot-cutover-complete has unfrozen signing.
hot-cutover-switch:
	$(MAKE) SIGNER_BACKEND=pkcs11 hot-cutover-restart

hot-cutover-restart:
	@test "$(SIGNER_BACKEND)" = pkcs11 || { echo "use: make hot-cutover-switch"; exit 1; }
	$(COMPOSE) up -d --no-deps --force-recreate --wait wallet-service
	@# sweep-signer pins the Hot address once at start: recreate it so it pins the new one (from hsm-gateway now, SIGNER_BACKEND=pkcs11)
	$(COMPOSE) up -d --no-deps --force-recreate --wait sweep-signer
	$(COMPOSE) restart chain-watcher
	$(COMPOSE) restart withdrawal-service

hot-cutover-verify: hot-cutover-image
	$(CUTOVER) verify
	@echo "consumers: wallet-service must now serve the new Hot address on GET /v1/wallet/hot-address"
	@got=$$(curl -sf --resolve wallet-service:8443:127.0.0.1 --cacert ../infra/pki/out/sweeper/ca-cert.pem --cert ../infra/pki/out/sweeper/cert.pem \
		--key ../infra/pki/out/sweeper/key.pem https://wallet-service:8443/v1/wallet/hot-address | sed -n 's/.*"address":"\([^"]*\)".*/\1/p'); \
	echo "  hot-address = $$got, expected $(NEW_HOT)"; \
	[ "$$(echo $$got | tr A-F a-f)" = "$$(echo $(NEW_HOT) | tr A-F a-f)" ] || { echo "FAIL: wallet-service does not serve the new Hot address"; exit 1; }
	@$(COMPOSE) logs wallet-service 2>&1 | grep -q 'Hot Key: hsm-gateway at' && echo "  wallet-service pinned the Hot address from hsm-gateway (no hot_wallet seed)" \
		|| { echo "FAIL: no 'Hot Key: hsm-gateway' line in wallet-service's log"; exit 1; }

# Step 9 (after the restarts): unfreeze, then start the sweeper (a fresh process reads the new Hot address).
hot-cutover-complete: hot-cutover-image
	$(CUTOVER) complete -switched
	$(if $(DRY_RUN),,$(COMPOSE) up -d --no-deps sweeper)

# Before the point of no return only: unfreeze and start the sweeper again.
hot-cutover-abort: hot-cutover-image
	$(CUTOVER) abort
	$(if $(DRY_RUN),,$(COMPOSE) up -d --no-deps sweeper)

# ---- the local rehearsal ---------------------------------------------------------------------------------------------
cutover-certs:
	@test -f "$${CA_DIR:-$$HOME/.digiwal-ca}/ca-key.pem" || ../infra/pki/gen-ca.sh
	@for svc in $(SERVICES) hsm-gateway hot-cutover auditor; do test -f ../infra/pki/out/$$svc/cert.pem || ../infra/pki/gen-leaf.sh $$svc; done
	@../infra/pki/fix-perms.sh $(addprefix ../infra/pki/out/,$(SERVICES) hsm-gateway hot-cutover auditor)

# Step 1 of the procedure for this throwaway stack: the ceremony (token + new Hot Key inside SoftHSM, policy, pinned
# caller) and hsm-gateway up with its self-test passed. The software Hot stays the Hot key until the switch.
cutover-hsm:
	$(MAKE) SIGNER_BACKEND=pkcs11 hsm-setup
	$(MAKE) SIGNER_BACKEND=pkcs11 cutover-gateway-up
	@echo "new Hot address (HSM): $$(cat $(HSM_DIR)/hot-address)"

.PHONY: cutover-gateway-up
cutover-gateway-up:
	$(COMPOSE) up -d --build hsm-gateway

# The auditor needs its database, its read-only logins and a Mongo login (services/auditor/roles).
cutover-auditor-roles:
	@$(COMPOSE) exec -T postgres psql -U custody -tc "SELECT 1 FROM pg_database WHERE datname='auditor'" | grep -q 1 || $(COMPOSE) exec -T postgres psql -U custody -c "CREATE DATABASE auditor"
	@P="$(COMPOSE) exec -T postgres psql -U custody -v ON_ERROR_STOP=1 -q"; \
	$$P -d postgres < ../services/auditor/roles/00_role.sql && $$P -d ledger < ../services/auditor/roles/ledger.sql && \
	$$P -d custody < ../services/auditor/roles/wallet.sql && $$P -d withdrawal < ../services/auditor/roles/withdrawal.sql && \
	$$P -d chain_watcher < ../services/auditor/roles/chain_watcher.sql
	@$(COMPOSE) exec -T mongo mongosh --quiet -u custody -p custody --authenticationDatabase admin < ../services/auditor/roles/mongo.js >/dev/null

# The rehearsal stack: the software Hot (SIGNER_BACKEND unset) exactly as `make up`, plus the auditor watching it.
cutover-up: cutover-certs
	@test "$(SIGNER_BACKEND)" = software || { echo "the rehearsal starts on the SOFTWARE Hot: unset SIGNER_BACKEND"; exit 1; }
	mkdir -p $(HSM_DIR)
	$(MAKE) up
	$(MAKE) cutover-auditor-roles
	$(COMPOSE) up -d --build --wait auditor

cutover-down:
	COMPOSE_PROFILES=hsm,audit,cutover $(COMPOSE) down -v --remove-orphans
	rm -rf $(HSM_DIR) $(SWEEP_DIR)

# Go phases of the rehearsal (cutover_test.go): before | frozen | after. State between them is $(HSM_DIR)/cutover-state.json.
cutover-test:
	E2E=1 E2E_CUTOVER=$(PHASE) E2E_CUTOVER_STATE=$(abspath $(HSM_DIR))/cutover-state.json go test -count=1 -v -timeout 12m -run 'TestCutover' ./...

# Operator capital the auditor's reconciliation must not count against the ledger: the Warm address is Anvil's account #9
# (10000 ETH, infra/treasury.yaml) and the rehearsal floats the Hot with 40 ETH and 3000 TT (cutover_test.go).
export RECON_CAPITAL ?= ETH=10040000000000000000000,TT=3000000000000000000000

AUDIT_PSQL = $(COMPOSE) exec -T postgres psql -U custody -d auditor -tA
OPEN_FINDINGS = SELECT rule || ' ' || reference FROM suspicious_transactions WHERE status='open' AND severity IN ('warn','critical') ORDER BY 1

# The auditor on the software Hot: what it already reports before anything is cut over is the baseline (the rehearsal must
# not add to it). Prints job results and the open warn/critical findings.
cutover-audit-baseline:
	@echo "waiting for the auditor to run every job on the software Hot..."; sleep 25
	@$(AUDIT_PSQL) -c "SELECT job || ' ' || result || ' ' || findings FROM job_runs WHERE id IN (SELECT max(id) FROM job_runs WHERE finished_at IS NOT NULL GROUP BY job) ORDER BY job"
	@$(AUDIT_PSQL) -c "$(OPEN_FINDINGS)" > $(HSM_DIR)/audit-baseline.txt; sort -o $(HSM_DIR)/audit-baseline.txt $(HSM_DIR)/audit-baseline.txt; echo "baseline open warn/critical findings: $$(wc -l < $(HSM_DIR)/audit-baseline.txt)"; cat $(HSM_DIR)/audit-baseline.txt

# The auditor, restarted so it learns the new Hot address from wallet-service, must report nothing beyond the baseline,
# and its treasury, reconciliation and ledger jobs must have run ok on the new Hot address.
cutover-audit:
	$(COMPOSE) restart auditor
	@echo "waiting for the auditor to run every job on the new Hot address..."; sleep 25
	@$(AUDIT_PSQL) -c "SELECT job || ' ' || result || ' ' || findings FROM job_runs WHERE id IN (SELECT max(id) FROM job_runs WHERE finished_at IS NOT NULL GROUP BY job) ORDER BY job"
	@$(AUDIT_PSQL) -c "$(OPEN_FINDINGS)" > $(HSM_DIR)/audit-now.txt; sort -o $(HSM_DIR)/audit-now.txt $(HSM_DIR)/audit-now.txt
	@# D7 is excluded from the comparison: the auditor's deposit scan flags every Gas Top-Up the Hot sends to a deposit address
	@# (chain-watcher skips Hot Wallet Outflow and the auditor does not), already on the software Hot, cutover or not.
	@new=$$(comm -13 $(HSM_DIR)/audit-baseline.txt $(HSM_DIR)/audit-now.txt | grep -v '^D7 ' || true); \
	echo "open warn/critical findings now: $$(wc -l < $(HSM_DIR)/audit-now.txt) ($$(grep -c '^D7 ' $(HSM_DIR)/audit-now.txt || true) of them D7 on Gas Top-Ups), new non-D7 since the baseline: $$(printf '%s' "$$new" | grep -c . || true)"; \
	if [ -n "$$new" ]; then echo "NEW FINDINGS:"; echo "$$new"; $(AUDIT_PSQL) -c "SELECT flow, rule, severity, reference, detail FROM suspicious_transactions WHERE status='open' AND severity IN ('warn','critical')"; exit 1; fi
	@bad=$$($(AUDIT_PSQL) -c "SELECT count(*) FROM job_runs WHERE id IN (SELECT max(id) FROM job_runs WHERE finished_at IS NOT NULL GROUP BY job) AND job IN ('treasury_audit','reconciliation','ledger_integrity') AND result <> 'ok'"); \
	[ "$$bad" = 0 ] || { echo "FAIL: $$bad of treasury_audit/reconciliation/ledger_integrity did not end ok"; exit 1; }
	@ran=$$($(AUDIT_PSQL) -c "SELECT count(DISTINCT job) FROM job_runs WHERE job IN ('treasury_audit','reconciliation','ledger_integrity') AND finished_at IS NOT NULL"); \
	[ "$$ran" = 3 ] || { echo "FAIL: only $$ran of treasury_audit/reconciliation/ledger_integrity ran"; exit 1; }

cutover-rehearse:
	$(MAKE) cutover-test PHASE=before
	$(MAKE) cutover-audit-baseline
	$(MAKE) cutover-hsm
	@echo "=== step 2: freeze (dry run first, then for real, twice) ==="
	$(MAKE) hot-cutover-freeze DRY_RUN=1
	$(MAKE) hot-cutover-freeze
	$(MAKE) hot-cutover-freeze
	$(call rpc,evm_setIntervalMining,[1])
	$(MAKE) cutover-test PHASE=frozen
	@echo "=== step 3: settle (stuck and in-flight txs of the old Hot address) ==="
	$(MAKE) hot-cutover-settle DRY_RUN=1
	$(MAKE) hot-cutover-settle
	$(MAKE) hot-cutover-settle
	@echo "=== steps 5-7: drain: dry run, then the point of no return, then again (idempotent) ==="
	$(MAKE) hot-cutover-drain DRY_RUN=1
	$(MAKE) hot-cutover-drain
	$(MAKE) hot-cutover-drain
	@echo "=== steps 8-9: switch wallet-service to the HSM key, restart consumers, verify, unfreeze ==="
	$(MAKE) hot-cutover-switch
	$(MAKE) hot-cutover-verify
	$(MAKE) hot-cutover-verify
	$(MAKE) hot-cutover-complete
	$(MAKE) hot-cutover-complete
	$(call rpc,evm_setIntervalMining,[0])
	$(call rpc,evm_setAutomine,[true])
	$(MAKE) cutover-test PHASE=after
	$(MAKE) cutover-audit

e2e-cutover:
	$(MAKE) cutover-up
	rc=0; $(MAKE) cutover-rehearse || rc=$$?; $(MAKE) cutover-down; exit $$rc
