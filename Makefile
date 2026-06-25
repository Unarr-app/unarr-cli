.PHONY: all build test lint arch coverage clean fmt vet check install-hooks changelog release release-patch release-minor release-major release-dry ship ship-dry ship-push

BINARY = unarr
SENTRY_DSN ?=
LDFLAGS = -s -w -X github.com/Unarr-app/unarr-cli/internal/sentry.dsn=$(SENTRY_DSN)

all: fmt vet lint arch test build

## Build the binary (stripped, ~28MB)
build:
	go build -ldflags '$(LDFLAGS)' -trimpath -o $(BINARY) ./cmd/unarr/


## Run all tests
test:
	go test -v -race -count=1 ./...

## Run linter (requires golangci-lint)
lint:
	golangci-lint run ./...

## Architectural / SOLID gate — file size (<500), func length, cyclomatic + cognitive
## complexity (15), nesting, dup, max-params (5). Scoped to NEW/CHANGED code vs origin/main
## (legacy god-files grandfathered). Keeps the LLM from producing spaghetti / god-files.
arch:
	@bash scripts/check-arch.sh
	@golangci-lint run -c .golangci.arch.yml ./...

## Run tests with coverage report (excludes CLI layer — cmd/ is glue code)
COVER_PKGS = $(shell go list ./... | grep -v '/cmd')
coverage:
	go test -race -coverprofile=coverage.out -covermode=atomic $(COVER_PKGS)
	@echo "──────────────────────────────────────"
	@go tool cover -func=coverage.out | tail -1
	@echo "──────────────────────────────────────"
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

## Preview changelog for next release
changelog:
	@git-cliff --unreleased --strip header

## Create a release: make release-patch, release-minor, release-major, or release V=0.5.0
release:
	@test -n "$(V)" || { echo "Usage: make release V=0.5.0"; exit 1; }
	@./scripts/release.sh $(V)

release-patch:
	@./scripts/release.sh patch

release-minor:
	@./scripts/release.sh minor

release-major:
	@./scripts/release.sh major

## Preview release without making changes
release-dry:
	@test -n "$(V)" || { echo "Usage: make release-dry V=patch|minor|major|0.5.0"; exit 1; }
	@./scripts/release.sh --dry-run $(V)

## Ship a release end-to-end (goreleaser + Hetzner + Docker Hub). Standalone backup for GH Actions.
## Reads version from internal/cmd/version.go unless V= is provided.
ship:
	@./scripts/ship.sh $(V)

## Ship + git push tag to GH afterwards
ship-push:
	@./scripts/ship.sh --push $(V)

## Preview ship steps without executing
ship-dry:
	@./scripts/ship.sh --dry-run $(V)

## Remove generated files
clean:
	rm -f $(BINARY) coverage.out coverage.html
