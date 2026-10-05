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

FROM alpine:3.20
RUN adduser -D -H app
USER app
COPY --from=build /out/service /usr/local/bin/service
EXPOSE 8080 8443
ENTRYPOINT ["service"]
