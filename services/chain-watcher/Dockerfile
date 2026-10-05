# Build context is the REPO ROOT (it needs pkg/events and pkg/metrics):
#   docker build -f services/chain-watcher/Dockerfile .
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY pkg/events pkg/events
COPY pkg/metrics pkg/metrics
COPY services/chain-watcher/go.mod services/chain-watcher/go.sum services/chain-watcher/
WORKDIR /src/services/chain-watcher
RUN go mod download
COPY services/chain-watcher/ .
RUN CGO_ENABLED=0 go build -o /out/service ./cmd/service

FROM alpine:3.20
RUN adduser -D -H app
USER app
COPY --from=build /out/service /usr/local/bin/service
EXPOSE 8080
ENTRYPOINT ["service"]
