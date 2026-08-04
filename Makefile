.PHONY: all build test test-e2e lint arch coverage clean fmt vet check install-hooks changelog release release-patch release-minor release-major release-dry

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

## Run the end-to-end suites in test/e2e (log ring under a foreign descriptor, config
## round-trip, the `unarr logs` CLI over a real process).
##
## They sit behind `//go:build e2e`, so plain `go test ./...` does NOT match them — the
## tag is the only way in, and without this target they never run at all. Kept out of
## `test` and out of the lefthook pre-commit hook on purpose: the suite compiles the CLI
## binary, which is ~4s with a warm build cache but ~50s cold, and a pre-commit hook that
## can stall for a minute is a hook people disable. CI runs it as its own job instead
## (.github/workflows/ci.yml), so it gates merges without taxing every commit.
test-e2e:
	go test -tags e2e -race -count=1 ./test/e2e/...

## Run linter (requires golangci-lint)
lint:
	golangci-lint run ./...

## Architectural / SOLID gate — file size (<500), func length, cyclomatic + cognitive
## complexity (15), nesting, dup, max-params (5). Scoped to NEW/CHANGED code vs the base
## rev below (legacy god-files grandfathered). Keeps the LLM from producing spaghetti.
##
## The base rev is resolved here rather than hardcoded in .golangci.arch.yml: golangci-lint
## does NOT error on an unresolvable new-from-rev, it silently falls back to reporting the
## ENTIRE legacy codebase (~165 issues), which reads as a catastrophic regression and makes
## the gate unusable. Clones name the remote differently (origin vs github), so try both
## before falling back to the local branch. Empty means "no base" → check-arch.sh degrades
## to checking every file, and golangci-lint is run unscoped.
ARCH_BASE := $(shell git rev-parse --verify --quiet origin/main \
	|| git rev-parse --verify --quiet github/main \
	|| git rev-parse --verify --quiet main)

arch:
	@bash scripts/check-arch.sh $(ARCH_BASE)
	@golangci-lint run -c .golangci.arch.yml $(if $(ARCH_BASE),--new-from-rev=$(ARCH_BASE),) ./...

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

# Releases run entirely on GitHub Actions (.github/workflows/release.yml): a
# vX.Y.Z tag push triggers goreleaser (cross-compile + ffmpeg bundle + ed25519
# sign + GitHub Release upload) and the multi-arch Docker Hub push. The web reads
# the latest version from the GitHub Releases API and the self-updater fetches
# signed binaries from GitHub Releases directly. Release flow:
#   1) make release V=x.y.z        # bump internal/cmd/version.go + CHANGELOG + tag vX
#   2) git push <github-remote> main --follow-tags   # tag push runs release.yml
# The old self-hosted ship pipeline (goreleaser + Hetzner version.txt mirror +
# Docker push via scripts/ship.sh) was removed once CI became the single path.

## Remove generated files
clean:
	rm -f $(BINARY) coverage.out coverage.html
