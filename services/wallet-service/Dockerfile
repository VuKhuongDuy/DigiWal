# Build context is the REPO ROOT (it needs pkg/metrics):
#   docker build -f services/wallet-service/Dockerfile .
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY pkg/metrics pkg/metrics
COPY services/wallet-service/go.mod services/wallet-service/go.sum services/wallet-service/
WORKDIR /src/services/wallet-service
RUN go mod download
COPY services/wallet-service/ .
RUN CGO_ENABLED=0 go build -o /out/service ./cmd/service
# The Hot cutover tool opens the legacy software Hot key (key-custody-hsm.md 5.5): `!prod` build tag, its own image stage
# below, and never in the stage that ships. `--build-arg TAGS=prod` skips it altogether.
ARG TAGS=""
RUN if [ "$TAGS" != "prod" ]; then CGO_ENABLED=0 go build -o /out/hot-cutover ./cmd/hot-cutover; fi
# key-admin is the production ceremony tool of the vault (key-custody-hsm.md M3c): built ALWAYS, its own image stage, and with the `prod`
# tag by default so the LEGACY commands (old MASTER_KEY software path: migrate-xpub, convert-seed, init-seed) are compiled out.
# `--build-arg KEYADMIN_TAGS=` (empty) builds them in, for the dev regression tooling only.
ARG KEYADMIN_TAGS=prod
RUN CGO_ENABLED=0 go build -tags "$KEYADMIN_TAGS" -o /out/key-admin ./cmd/key-admin

# docker build --target cutover -t hot-cutover:local -f services/wallet-service/Dockerfile .   (operator-only, one-off)
FROM alpine:3.20 AS cutover
RUN adduser -D -H app
USER app
COPY --from=build /out/hot-cutover /usr/local/bin/hot-cutover
ENTRYPOINT ["hot-cutover"]

# docker build --target keyadmin -t key-admin:local -f services/wallet-service/Dockerfile .   (operator-only, one-off)
FROM alpine:3.20 AS keyadmin
RUN adduser -D -H app
USER app
COPY --from=build /out/key-admin /usr/local/bin/key-admin
ENTRYPOINT ["key-admin"]

FROM alpine:3.20
RUN adduser -D -H app
USER app
COPY --from=build /out/service /usr/local/bin/service
EXPOSE 8080 8443
ENTRYPOINT ["service"]
