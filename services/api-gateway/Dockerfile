# Build context is the REPO ROOT (it needs pkg/metrics):
#   docker build -f services/api-gateway/Dockerfile .
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY pkg/metrics pkg/metrics
COPY services/api-gateway/go.mod services/api-gateway/go.sum services/api-gateway/
WORKDIR /src/services/api-gateway
RUN go mod download
COPY services/api-gateway/ .
RUN CGO_ENABLED=0 go build -o /out/service ./cmd/service

FROM alpine:3.20
RUN adduser -D -H app
USER app
COPY --from=build /out/service /usr/local/bin/service
EXPOSE 8080
ENTRYPOINT ["service"]
