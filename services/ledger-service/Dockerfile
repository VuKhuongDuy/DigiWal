# Build context is the REPO ROOT (it needs pkg/events and pkg/metrics):
#   docker build -f services/ledger-service/Dockerfile .
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY pkg/events pkg/events
COPY pkg/metrics pkg/metrics
COPY services/ledger-service/go.mod services/ledger-service/go.sum services/ledger-service/
WORKDIR /src/services/ledger-service
RUN go mod download
COPY services/ledger-service/ .
RUN CGO_ENABLED=0 go build -o /out/service ./cmd/service

FROM alpine:3.20
RUN adduser -D -H app
USER app
COPY --from=build /out/service /usr/local/bin/service
EXPOSE 8080 8443
ENTRYPOINT ["service"]
