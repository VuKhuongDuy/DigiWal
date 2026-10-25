# Build context is the REPO ROOT (it needs pkg/metrics):
#   docker build -f services/sweep-signer/Dockerfile .
#
# key-custody-hsm.md invariant 16: a static binary on `scratch`: no shell, no package manager, no libc, no /etc, nothing to
# `exec` into. The uid is numeric and fixed (10002); there is no passwd file because nothing needs one.
# TAGS=prod (the compose default for production overlays) compiles out the DEV-only Hot address source (HOT_SOURCE=wallet-service).
FROM golang:1.26-alpine AS build
ARG TAGS=""
WORKDIR /src
COPY pkg/metrics pkg/metrics
COPY services/sweep-signer/go.mod services/sweep-signer/go.sum services/sweep-signer/
WORKDIR /src/services/sweep-signer
RUN go mod download
COPY services/sweep-signer/ .
RUN CGO_ENABLED=0 go build -trimpath -tags "$TAGS" -ldflags="-s -w" -o /out/service ./cmd/service

FROM scratch
COPY --from=build /out/service /service
USER 10002:10002
EXPOSE 8080 8443
ENTRYPOINT ["/service"]
