.PHONY: all build test lint coverage clean fmt vet check install-hooks

BINARY = unarr

all: fmt vet lint test build

## Build the binary
build:
	go build -o $(BINARY) ./cmd/unarr/

## Run all tests
test:
	go test -v -race -count=1 ./...

## Run linter (requires golangci-lint)
lint:
	golangci-lint run ./...

## Run tests with coverage report
coverage:
	go test -race -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -func=coverage.out
	go tool cover -html=coverage.out -o coverage.html

## Format code
fmt:
	gofmt -s -w .

## Check formatting (no write, exits non-zero if unformatted)
check:
	@test -z "$$(gofmt -l .)" || { echo "Files not formatted:"; gofmt -l .; exit 1; }

## Run go vet
vet:
	go vet ./...

## Install lefthook git hooks
install-hooks:
	lefthook install

## Install binary to GOPATH/bin
install:
	go install ./cmd/unarr/

## Remove generated files
clean:
	rm -f $(BINARY) coverage.out coverage.html
