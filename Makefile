.PHONY: build check test clean

build:
	go build -trimpath -o bin/inf-backup ./cmd/inf-backup

check:
	@test -z "$$(gofmt -l cmd internal)"
	go vet ./...
	go test -race ./...
	sh -n entrypoint-posix.sh scripts/consumer-contract.sh
	sh scripts/consumer-contract.sh

test:
	go test -race ./...

clean:
	rm -f bin/inf-backup
