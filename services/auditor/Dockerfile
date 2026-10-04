# Build context is the REPO ROOT (same as the other services):
#   docker build -f services/auditor/Dockerfile .
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY services/auditor/go.mod services/auditor/go.sum services/auditor/
WORKDIR /src/services/auditor
RUN go mod download
COPY services/auditor/ .
RUN CGO_ENABLED=0 go build -o /out/service ./cmd/service

FROM alpine:3.20
RUN adduser -D -H app
USER app
COPY --from=build /out/service /usr/local/bin/service
EXPOSE 8080
ENTRYPOINT ["service"]
