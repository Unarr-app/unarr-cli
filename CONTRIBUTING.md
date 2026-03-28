# Contributing to unarr

Thank you for your interest in contributing! This guide will help you get started.

## Getting Started

1. **Fork** the repository on GitHub
2. **Clone** your fork locally:
   ```bash
   git clone https://github.com/YOUR-USERNAME/torrentclaw-cli.git
   cd torrentclaw-cli
   ```
3. **Set up the Go client** (local dependency):
   ```bash
   # Clone the go-client next to the CLI
   cd ..
   git clone https://github.com/torrentclaw/go-client.git
   cd torrentclaw-cli
   ```
4. **Create a branch** for your change:
   ```bash
   git checkout -b feature/my-feature
   ```
5. **Make your changes**, write tests, and ensure everything passes
6. **Commit** with a clear message (see [Commit Messages](#commit-messages))
7. **Push** to your fork and [open a Pull Request](https://github.com/torrentclaw/torrentclaw-cli/compare)

## Development Setup

You need **Go 1.22+** installed.

### Build and Run

```bash
make build          # Build the binary
./unarr --help  # Test it

# Or install to GOPATH/bin
make install
```

### Git Hooks (Lefthook)

This project uses [Lefthook](https://github.com/evilmartians/lefthook) to run pre-commit checks and validate commit messages automatically.

```bash
# Install lefthook (pick one):
brew install lefthook          # macOS
go install github.com/evilmartians/lefthook@latest  # Go
npm install -g lefthook        # npm

# Activate hooks in your local clone:
make install-hooks
# or: lefthook install
```

Once installed, every commit will automatically:
- **pre-commit**: check `gofmt`, run `go vet`, build, and run `golangci-lint` (if installed)
- **commit-msg**: validate the message follows [Conventional Commits](#commit-messages)

### Make Targets

```bash
make build      # Build the binary
make test       # Run tests
make coverage   # Run tests with coverage
make lint       # Run golangci-lint
make fmt        # Format code (gofmt -s -w)
make check      # Verify formatting (no write, CI-friendly)
make vet        # Run go vet
make install    # Install to GOPATH/bin
make all        # fmt + vet + lint + test + build
make install-hooks  # Install lefthook git hooks
```

## Project Structure

```
torrentclaw-cli/
├── cmd/unarr/           # Entry point
│   └── main.go
├── internal/
│   ├── cmd/             # Cobra command definitions
│   │   ├── root.go      # Root command + global flags
│   │   ├── search.go    # Search command
│   │   ├── inspect.go   # Inspect (TrueSpec) command
│   │   ├── watch.go     # Watch (streaming + torrents)
│   │   ├── popular.go   # Popular/recent content
│   │   ├── config.go    # Interactive configuration
│   │   ├── setup.go     # First-time setup wizard
│   │   ├── daemon.go    # Daemon mode (start/stop)
│   │   ├── download.go  # One-shot download
│   │   ├── stream.go    # Stream to media player
│   │   ├── doctor.go    # Diagnostics
│   │   ├── status.go    # Daemon status
│   │   └── stubs.go     # Stub commands (future)
│   ├── config/          # Configuration management
│   │   ├── config.go    # Config struct + TOML parsing
│   │   └── paths.go     # XDG-compliant paths
│   ├── engine/          # Download engine
│   │   ├── manager.go   # Download orchestration
│   │   ├── task.go      # Task state machine
│   │   ├── torrent.go   # BitTorrent downloader
│   │   ├── stream.go    # Streaming engine
│   │   ├── organize.go  # File organization
│   │   └── ...          # Verify, resolve, notify, etc.
│   ├── agent/           # API client + daemon
│   │   ├── client.go    # HTTP client
│   │   └── daemon.go    # Daemon lifecycle
│   ├── ui/              # Output formatting
│   │   ├── table.go     # Table rendering
│   │   └── format.go    # Size, rating, time formatting
│   └── parser/          # Torrent parsing
│       └── torrent.go   # Magnet URI, hash, name parsing
├── go.mod
├── Makefile
└── README.md
```

## Code Style

- Run `gofmt` on all code (or `make fmt`)
- Run `golangci-lint` (or `make lint`)
- Follow existing patterns in the codebase
- Keep commands in separate files under `internal/cmd/`
- Keep output formatting in `internal/ui/`

## Running Tests

```bash
# All tests
make test

# Specific test
go test -run TestParse -v ./internal/parser/...

# With coverage report
make coverage
```

## Commit Messages

This project enforces [Conventional Commits](https://www.conventionalcommits.org/) via a git hook. Format:

```
<type>[optional scope]: <description>
```

Allowed types: `feat`, `fix`, `docs`, `test`, `chore`, `refactor`, `ci`, `style`, `perf`, `build`

Examples:

```
feat: add search by genre
feat(inspect): add HDR detection
fix(search): handle empty results
docs: update README
test: add parser edge case tests
chore: update CI matrix to Go 1.24
```

## Pull Request Guidelines

- Keep PRs focused — one feature or fix per PR
- Include tests for new functionality
- Update documentation if the public API changes
- Ensure all CI checks pass before requesting review
- Link related issues in the PR description

## Adding a New Command

1. Create `internal/cmd/mycommand.go`
2. Define a `newMyCommandCmd() *cobra.Command` function
3. Add it to `rootCmd.AddCommand(...)` in `root.go`
4. Add any UI rendering helpers to `internal/ui/table.go`

## Reporting Bugs

[Open an issue](https://github.com/torrentclaw/torrentclaw-cli/issues/new?labels=bug) with:

- **Description** — what went wrong
- **Steps to reproduce** — minimal commands to trigger the bug
- **Expected behavior** — what you expected to happen
- **Actual behavior** — what actually happened
- **Environment** — Go version, OS, CLI version (`unarr version`)

## Code of Conduct

This project follows the [Contributor Covenant v2.1](https://www.contributor-covenant.org/version/2/1/code_of_conduct/).

In short:

- **Be respectful** — treat everyone with dignity regardless of background or experience level
- **Be constructive** — focus on what's best for the project and community
- **Be collaborative** — welcome newcomers, help others learn
- **No harassment** — unacceptable behavior includes trolling, insults, and unwelcome attention

---

Thank you for helping make unarr better!
