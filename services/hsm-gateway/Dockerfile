# Build context is the REPO ROOT (it needs pkg/metrics):
#   docker build -f services/hsm-gateway/Dockerfile .
#
# debian-slim, NOT alpine: M0 spike (f) built and passed on both, but alpine/musl is ~2x slower on the sign
# path (p99 9.7 ms vs 3.7-5.2 ms at 16 goroutines) and SoftHSMv2 is only exercised upstream on glibc.
# The binary is built WITH cgo (miekg/pkcs11 dlopens libsofthsm2.so); wallet-service stays CGO_ENABLED=0 and
# has no PKCS#11 library in its image (key-custody-hsm.md invariant 2).

# ---- test environment: Go + gcc + softhsm2. `make test-softhsm` builds this and runs `go test -tags softhsm`.
FROM golang:1.26-bookworm AS testenv
RUN apt-get update && apt-get install -y --no-install-recommends softhsm2 gcc libc6-dev ca-certificates \
    && rm -rf /var/lib/apt/lists/*
ENV CGO_ENABLED=1
WORKDIR /src/services/hsm-gateway

# ---- build
FROM golang:1.26-bookworm AS build
RUN apt-get update && apt-get install -y --no-install-recommends gcc libc6-dev && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY pkg/metrics pkg/metrics
COPY services/hsm-gateway/go.mod services/hsm-gateway/go.sum services/hsm-gateway/
WORKDIR /src/services/hsm-gateway
RUN go mod download
COPY services/hsm-gateway/ .
RUN CGO_ENABLED=1 go build -trimpath -o /out/service ./cmd/service

# ---- runtime
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends softhsm2 ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 10001 --no-create-home --shell /usr/sbin/nologin hsmgw \
    && mkdir -p /var/lib/softhsm/tokens /var/lib/hsm-gateway/audit /var/lib/hsm-gateway/state \
    && chown -R hsmgw:hsmgw /var/lib/softhsm /var/lib/hsm-gateway && chmod -R 0700 /var/lib/softhsm /var/lib/hsm-gateway
COPY services/hsm-gateway/config/softhsm2.conf /etc/softhsm/softhsm2.conf
# Debian's softhsm2 package makes /etc/softhsm root:softhsm 0750, so uid 10001 could not read the config
# (softhsm2-util and init-token failed with CKR_GENERAL_ERROR). Found by the M2b integration test.
RUN chmod 0755 /etc/softhsm && chmod 0644 /etc/softhsm/softhsm2.conf
COPY --from=build /out/service /usr/local/bin/service
ENV SOFTHSM2_CONF=/etc/softhsm/softhsm2.conf
USER hsmgw
EXPOSE 8080 8443
ENTRYPOINT ["service"]
