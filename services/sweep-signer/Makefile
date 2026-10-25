.PHONY: test vet build
vet:
	go vet ./...
test:
	go test -race -count=1 ./...
build:
	go build -tags prod -o /dev/null ./cmd/service && go build -o /dev/null ./cmd/service
