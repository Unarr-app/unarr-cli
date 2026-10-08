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

## Native remote mount acceptance

Use task/SHA-specific native binaries with the seven-argument
`remote-mount-native.sh SOURCE_SHA TASK_TAG CLI CMD_TEST SETUP_TEST NNTP_TEST YENC_TEST`.
It refuses an existing artifact directory, records OS/binary hashes, verifies
official rclone, guards real labels/plists before and after, and records explicit
PASS/FAIL/SKIP. Cross-compilation alone does not establish native acceptance.

Mac persistence uses a **fixture-prepared private service environment**. The
production launchd installer does not propagate an invoking shell's HOME.
This fixture never calls public `daemon install`: it prepares a private canonical
plist pointing at the actual CLI with explicit private HOME/PATH/XDG/temporary
paths and blank forbidden configuration overrides. Public mount sees this
already prepared definition. This covers the actual persistent controller and
mount lifecycle; it does not validate installer environment propagation.

With explicit `UNARR_NATIVE_MAC_SANDBOX=1`, the private definition launches the actual CLI through the existing system
`sandbox-exec` with a private profile that allows only loopback outbound network.
This contains the ordinary daemon's idle torrent/DHT startup as well as its
children. An actual sandboxed child verifies loopback HTTP succeeds and a UDP
write to a reserved nonloopback address is denied by permission before bootstrap.
The system launcher and profile inode/content are pinned alongside the plist;
native process identity still verifies the actual CLI after exec handoff.
This process sandbox is fixture preparation, not a global security-policy change
or public installer behavior. Real-provider acceptance remains outside scope.

The standard fixture launches the actual CLI directly because macOS rejects
execution of the installed setuid macFUSE helper inside this process sandbox.
It still uses exclusively synthetic loopback API/CDN/auth fixtures, no real
accounts or queued torrents, and private state. Ordinary idle torrent/DHT
initialization is not denied in this mode; it does not prove total network
isolation. Preserve sandbox failures separately rather than labeling them PASS.
Bounded private daemon/rclone logs are retained in test output before cleanup.

Before preparation, native id/DirectoryServices corroborate os/user UID and the
real home. Both canonical/legacy real-home plists and GUI/user registrations
must be absent; root/inaccessible sessions are refused. A UID lock excludes a
second fixture. The private HOME is canonicalized (including macOS /var alias),
owned0700, with no symlink ancestry. Plist parsing and a real child process prove
the declared default config/state/log/tools paths stay private before bootstrap.

With `UNARR_NATIVE_KERNEL=0`, `TestMountNativeMacPersistentPreparation` validates
the definition/environment only. `TestMountNativeMacPersistent` prepares and
cleans private files, then explicitly SKIPs before bootstrap/load even when
`UNARR_NATIVE_MAC_SERVICE=1`. The existing four unique-label launchd controller
tests remain separate. No prepared-service label is mutated by these new
kernel-off tests.

Actual mounted persistence requires **both** `UNARR_NATIVE_KERNEL=1` and
`UNARR_NATIVE_MAC_SERVICE=1`, an already-approved/loaded macFUSE extension, and
sole native session ownership. Neither the runner nor tests activate a driver,
approve security prompts, reboot or use global launchctl environment settings.
Use a fresh task tag and preserve the source/archive/binary hashes.

The mounted test checks exact listing/sizes/ranged bytes after the initiating
CLI exits; ready restart drains old daemon/rclone identities and a retained DAV
peer before new mounted reads; fresh K2 ranges keep the same daemon; both saved
disabled and ordinary umount remove DAV/child/mount while leaving a healthy
ordinary daemon. It remounts before actual uninstall so cleanup is verified
from a live mounted service. Synthetic second/third cycle consent is explicitly
saved; interactive consent coverage is SKIP.

Every public service mutator rechecks both domains, legacy absence, the current
private definition inode/content, program path/environment and process
identity. Cleanup refuses a foreign replacement and preserves the explicit
temporary tree for recovery; automatic t.TempDir cleanup cannot delete it.
Owned readers are joined after unmount, client sockets closed, and real guards
repeated. PID receipts use native ps creation time at seconds precision plus
the exact executable; no PID-only kill is used. The stable owned UID lock inode
is retained unlocked for reuse.
