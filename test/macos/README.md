# macOS launchd regression tests

Verified on the Mac mini, macOS 26.6.2 (25G83), Apple Silicon.

The reported `Load failed: 5: Input/output error` reproduces when the job is
already registered, and when launchd has persistently disabled it. In both cases
the legacy `launchctl load` command returns exit status **0**, so checking only
`exec.Run()` incorrectly reports success. A registered job is also not evidence
that its process stayed alive.

A new job in `user/<uid>` also rejects the default Aqua-only definition with
error 5. The generated plist explicitly permits Aqua and Background sessions;
the controller retains an existing user-domain registration instead of trying
to register the same label again in the GUI domain.

The regression suite uses explicit domains, modern launchctl commands, temporary
plists and unique labels. It covers repeated start/stop, delayed termination,
disabled jobs, SIGKILL recovery, missing plists, immediate crash loops and an
existing user-domain job while a GUI session is available.

Run on macOS:

```sh
UNARR_LAUNCHD_E2E=1 go test -race -count=1 ./internal/cmd -run '^TestLaunchdReal'
```

The optional CLI installation test runs the actual daemon against an HTTP API
fixture bound to loopback, with temporary config/log/download directories and
no production credentials. It tests install/reinstall, CLI controls, legacy
`app.unarr.daemon` migration, crash recovery, a failed startup followed by config
repair, and repeated uninstall. An executable path containing spaces and `&`
also verifies plist XML escaping.

Use a disposable account with no unarr daemon installed. The test refuses to run
if either supported plist exists in the real home or either label is registered.
The temporary HOME isolates files, but launchd labels still belong to the real
user's session. Do not run this test concurrently in that account.

```sh
go build -o /tmp/unarr-launchd-cli ./cmd/unarr
UNARR_LAUNCHD_E2E=1 UNARR_LAUNCHD_CLI=/tmp/unarr-launchd-cli \
  go test -race -count=1 ./internal/cmd ./internal/service ./internal/agent ./cmd/unarr-desktop
```

All jobs are stopped during cleanup. These tests establish recovery from the
reported launchd failure; without the reporter's crash logs they cannot identify
an independent cause for every process exit on that machine.
