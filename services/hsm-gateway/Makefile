# docker-compose.yml pulls in ../../infra/docker-compose.yml via `include:` — don't also pass it with -f.
COMPOSE = docker compose
ROOT   := $(abspath ../..)
IMAGE_TEST = hsm-gateway-test

.PHONY: test test-softhsm vet build image secrets init-token audit-verify certs

# Unit tests that need no HSM (a software stand-in lives only in _test files). cgo is required: the pkcs11 binding is cgo.
test:
	go vet ./...
	go test -race ./...

# The honest integration run: a REAL SoftHSMv2 inside a Docker container (the host has no softhsm), the whole suite
# with -tags softhsm and -race: init-token, PIN guard, bad key attributes, 10 000-digest property test against the
# token, concurrent signing, simulated EVM mining HSM signatures, SIGKILL mid-sign. Needs Docker and network (modules).
test-softhsm:
	docker build -f Dockerfile --target testenv -t $(IMAGE_TEST) $(ROOT)
	docker run --rm -v $(ROOT):/src -v hsm-gateway-gocache:/root/go -v hsm-gateway-gobuild:/root/.cache/go-build \
		-w /src/services/hsm-gateway $(IMAGE_TEST) \
		sh -c 'go vet -tags softhsm ./... && go test -tags softhsm -race -count=1 -v ./...'

vet:
	go vet ./... && go vet -tags softhsm ./...

image:
	docker build -f Dockerfile -t hsm-gateway $(ROOT)

# Leaf certificate for the gateway (CN hsm-gateway), 0400 key is M4; today gen-leaf.sh writes 0644.
certs:
	@test -f "$${CA_DIR:-$$HOME/.digiwal-ca}/ca-key.pem" || ../../infra/pki/gen-ca.sh
	@test -f ../../infra/pki/out/hsm-gateway/cert.pem || ../../infra/pki/gen-leaf.sh hsm-gateway

# The user PIN, from a random value, into a file outside git; never printed, never an env var.
secrets:
	@mkdir -p secrets data/tokens data/audit data/state
	@test -f secrets/hsm-user-pin && echo "secrets/hsm-user-pin exists, not overwriting" || \
		(umask 077 && openssl rand -base64 18 | tr -d '\n=/+' | cut -c1-16 > secrets/hsm-user-pin && echo "wrote secrets/hsm-user-pin (mode 0600; chown to uid 10001 and chmod 0400 on the host)")

# The one-off ceremony: initialise the token and generate the Hot Key INSIDE it. The SO PIN is typed once, piped on stdin,
# never stored. Prints the Hot address to put into config/policy.yaml. Refuses to create a second Hot Key.
#   read -rs SO && printf '%s\n' "$SO" | make init-token
init-token:
	$(COMPOSE) run --rm -T hsm-gateway init-token

# Verify the audit chain; EXPECT_SEQ/EXPECT_HASH come from the auditor's recorded head (detects tail truncation).
#   make audit-verify EXPECT_SEQ=1234 EXPECT_HASH=...
audit-verify:
	$(COMPOSE) run --rm --no-deps hsm-gateway audit-verify $(if $(EXPECT_SEQ),-expect-seq $(EXPECT_SEQ)) $(if $(EXPECT_HASH),-expect-hash $(EXPECT_HASH))
