# Build context is the REPO ROOT (it needs pkg/events and pkg/metrics):
#   docker build -f services/sweeper/Dockerfile .
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY pkg/events pkg/events
COPY pkg/metrics pkg/metrics
COPY services/sweeper/go.mod services/sweeper/go.sum services/sweeper/
WORKDIR /src/services/sweeper
RUN go mod download
COPY services/sweeper/ .
RUN CGO_ENABLED=0 go build -o /out/service ./cmd/service

FROM alpine:3.20
RUN adduser -D -H app
USER app
COPY --from=build /out/service /usr/local/bin/service
EXPOSE 8080
ENTRYPOINT ["service"]
