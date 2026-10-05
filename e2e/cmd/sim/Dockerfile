# Build context is e2e/ (the sim is part of the e2e Go module):
#   docker build -f cmd/sim/Dockerfile .
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY contracts contracts
RUN CGO_ENABLED=0 go build -o /out/sim ./cmd/sim

FROM alpine:3.20
RUN adduser -D -H app && mkdir /data && chown app /data
USER app
COPY --from=build /out/sim /usr/local/bin/sim
ENV SIM_STATE_FILE=/data/state.json
ENTRYPOINT ["sim"]
CMD ["run"]
