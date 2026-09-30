# Build context is the REPO ROOT (it needs pkg/events):
#   docker build -f services/history-service/Dockerfile .
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY pkg/events pkg/events
COPY services/history-service/go.mod services/history-service/go.sum services/history-service/
WORKDIR /src/services/history-service
RUN go mod download
COPY services/history-service/ .
RUN CGO_ENABLED=0 go build -o /out/service ./cmd/service

FROM alpine:3.20
RUN adduser -D -H app
USER app
COPY --from=build /out/service /usr/local/bin/service
EXPOSE 8080
ENTRYPOINT ["service"]
