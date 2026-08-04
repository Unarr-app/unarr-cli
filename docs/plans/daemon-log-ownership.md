# Daemon log ownership — design

Status: design, ready to implement. No code written yet.
Scope: `internal/logging`, `internal/cmd` (daemon start, installers, VBS shim, `unarr logs`, `clean`).

## 1. The defect, restated from measurement

The Windows scheduled task runs a VBScript shim that launches:

```
cmd /c ""<bin>" start >> "<dir>\unarr.log" 2>&1
```

`cmd.exe` opens the redirect target with `dwShareMode = FILE_SHARE_READ`. Measured on the
repo's Win11 VM with the real binary:

```
unarr logs rotate -> exit 1
Error: truncate log file: open C:\unarrlab\unarr\unarr.log:
The process cannot access the file because it is being used by another process.
snapshot unarr.log.1 present = True (2108130 bytes)   <- the copy ALREADY happened
live unarr.log after the truncate = 2108228 bytes     <- it did not shrink
```

`RotateNow` does `shiftRotated` → `snapshot` → `truncate`. On Windows the first two succeed
and the third fails, and `janitor.go` swallows the error (`_ = RotateNow(opts)`). Per 60 s
tick at the 20 MB default: the ring shifts (real history is gone in 3 minutes), 20 MB is
copied (~28 GB/day of writes), and the live log never shrinks. Silently.

Why the third step is the one that fails, exactly: `os.Truncate` on Windows is

```go
// $GOROOT/src/os/file_windows.go:207
func Truncate(name string, size int64) error {
	f, e := OpenFile(name, O_WRONLY, 0666)   // GENERIC_WRITE
	...
}
```

`GENERIC_WRITE` is not in the share mode `cmd.exe` granted, so the open is a sharing
violation. On POSIX (launchd, the detached launcher) the holder is a real `O_APPEND`
descriptor, `syscall.Truncate` needs no new open, and copy-truncate is correct. **The defect
is exclusive to the Windows shim / scheduled-task path.**

## 2. The decision

The daemon becomes the **owner** of its own log: it opens the file itself with `O_APPEND`
(Go maps that to `FILE_APPEND_DATA` on Windows — `syscall_windows.go:393`) and rotation
becomes a **rename**, which works precisely because the process doing the rename holds the
descriptor.

The constraint that makes this non-trivial: **if the daemon owns the fd, nobody else may
hold that file.** Concretely on Windows, if the shim kept `>> unarr.log` while the daemon
also opened it, `cmd.exe`'s `FILE_SHARE_READ` would reject the daemon's `FILE_APPEND_DATA`
open and the daemon would have **no log at all** — strictly worse than today.

So the redirect does not disappear; it **moves to a second, different file**.

### Two files, two owners, two mechanisms

| file | owner | contents | bounded by |
|---|---|---|---|
| `unarr.log` (+ `.1 … .N`) | the **daemon** (`logging.Writer`) | everything through `log.Printf` | rename rotation, `log_max_size_mb` / `log_max_files` |
| `unarr.boot.log` (+ `.1`) | the **supervisor's redirect** | startup banner (`fmt`/`color` → stdout), cobra's fatal-start errors, raw Go panic dumps | copy-truncate janitor on POSIX; a size-checked rename inside the VBS shim on Windows |

The split is not cosmetic: a Go panic writes the goroutine dump straight to fd 2 and never
passes through `log.SetOutput`. The only way to keep it is for **someone else** to hold fd 2
pointed at a file. That someone is the supervisor, and the file it holds must not be the one
the daemon renames.

### How the daemon knows it should own the file

An explicit, per-invocation flag on `unarr start`:

```
unarr start --log-file <path>
```

Not an env var (a stray `UNARR_LOG_FILE` in a user's profile would silently mute a foreground
`unarr start`), not a heuristic (`isatty` is wrong under Docker, where output must stay on
stdout for `docker logs`). Empty flag = today's behaviour: write to stdout/stderr and let
whoever started the process decide. Every supervisor that used to redirect a file passes it;
systemd, Docker and a human in a terminal do not.

## 3. Platform / launcher matrix

### 3.1 Linux — systemd user unit — **NO CHANGE**

`--log-file` is **not** passed. `systemdTemplate` (`internal/cmd/daemon_install.go:22`) has no
`StandardOutput=`, so the daemon's fd 1/2 go to the journal, and `usesJournald()`
(`internal/cmd/logs.go:228`, `runtime.GOOS == "linux" && service.Respawns()`) makes
`unarr logs` shell out to `journalctl`. There is no `unarr.log` on this path at all; nothing
holds a descriptor to a file; `Sweep` is already a no-op because the file does not exist.

Justification for leaving it alone: making the daemon own a file here would empty the journal
and break `journalctl --user -u unarr -f` — the exact command `installSystemd` prints as the
way to read logs (`daemon_install.go:295`). journald already does the bounding job
(`SystemMaxUse`/vacuum) that this whole design is about. Adding a second, private, unrotated
log next to a working one is a regression with no upside.

### 3.2 Linux/BSD/NAS without systemd — detached start — **CHANGED**

`startDaemonDetached` (`internal/cmd/daemon_control.go:285`, reached from `internal/cmd/init.go:344`)
today opens `unarr.log` with `logging.OpenFile` and hands the `*os.File` to `cmd.Stdout` /
`cmd.Stderr`. Change: it opens the **boot log** instead and adds `--log-file <unarr.log>` to
the child's args. The holder of the boot log stays a Go `O_APPEND` file, so the copy-truncate
janitor bounds it correctly.

`tailLogFile(logPath, 15)` in the immediate-exit error path must tail the **boot log** — an
exit that fast happens before the Writer is installed, so `unarr.log` may not even exist.

Plain `unarr start` from cron `@reboot` / Synology Task Scheduler / unRAID `go` with a user's
own shell redirect: unchanged, no flag, daemon does not own anything, janitor copy-truncates
(POSIX `O_APPEND` — works).

### 3.3 macOS — launchd — **CHANGED, but not urgently**

`launchdTemplate` (`daemon_install.go:46`) gains `--log-file {{.LogDir}}/unarr.log` in
`ProgramArguments`, and `StandardOutPath`/`StandardErrorPath` both move to
`{{.LogDir}}/unarr.boot.log`. launchd holds the boot log for the agent's whole life with a
POSIX append descriptor, so the copy-truncate janitor bounds it exactly as it bounds
`unarr.log` today.

Migration note that must **not** be "fixed": nothing rewrites an installed plist until the
next `unarr daemon install` (unlike Windows, where `self_update.go:116` calls
`reregisterWindowsTaskAfterUpgrade`). A macOS box that upgrades the binary keeps the old
plist, therefore never passes `--log-file`, therefore stays on copy-truncate — **which works
on macOS**. That is an acceptable steady state, not a bug to chase.

### 3.4 Windows — scheduled task + VBS shim — **CHANGED, this is the fix**

`buildLauncherVBS` (`internal/cmd/daemon_launch_vbs.go:93`) changes the command it runs:

```
cmd /c ""<bin>" start --log-file "<dir>\unarr.log" >> "<dir>\unarr.boot.log" 2>&1"
```

(The no-redirect fallback form keeps `--log-file` too.) `cmd.exe` now holds only
`unarr.boot.log`; `unarr.log` is opened by the daemon alone and renamed by the daemon alone.

The boot log cannot be bounded by copy-truncate here — that is the measured bug. It is
bounded by the shim, in the window where **nothing** holds it (between `sh.Run` returning and
the next launch): before each launch, if `unarr.boot.log` is over budget, rename it to
`unarr.boot.log.1` (deleting any previous `.1`). Size-checked rather than unconditional so a
crash loop does not push the first, interesting crash out of the ring after two relaunches.

VBScript, inserted at the top of the `Do` loop, before `started = Timer`:

```vbscript
  Err.Clear
  If Not fso Is Nothing Then
    If fso.FileExists(bootLog) Then
      If fso.GetFile(bootLog).Size > bootMaxBytes Then
        If fso.FileExists(bootLog1) Then fso.DeleteFile bootLog1, True
        fso.MoveFile bootLog, bootLog1
      End If
    End If
  End If
  Err.Clear
```

`On Error Resume Next` is already in force at the top of the script, and every step is
best-effort: a boot log that cannot be rotated must never stop the daemon launching.

### 3.5 Docker — **NO CHANGE**

`Dockerfile` is `ENTRYPOINT docker-entrypoint.sh` + `CMD ["up"]`, and `up` reaches the same
`runDaemon` in the foreground with no `--log-file`. Output stays on stdout for `docker logs`.
Owning a file inside a container would hide the log from the only tool that reads it there.

### 3.6 Foreground `unarr start` in a terminal — **NO CHANGE**

No flag, no ownership, output on the tty. This is the case that rules out any tty/env-based
auto-detection: it must keep working byte-for-byte.

## 4. Bootstrap / crash output plan

The problem: `fmt.Print*` (the `  unarr Daemon` banner at `daemon.go:219`), cobra's error
print for a fatal start (no API key, no agent ID, instance lock held), and a Go panic dump
all bypass `log.SetOutput`. If the supervisor stops redirecting, they evaporate — and they
are exactly what a failed start needs.

**Plan.**

1. **File**: `unarr.boot.log` in `config.DataDir()`, one rotated slot `unarr.boot.log.1`.
   Budget is a **fixed 2 MB**, deliberately not the user's `log_max_size_mb`: this file holds
   banners and stack traces, and a user who raises the main log to 500 MB did not ask for a
   500 MB banner file.
2. **Who holds it**: the supervisor — `>>` from `cmd.exe` on Windows,
   `StandardOutPath`/`StandardErrorPath` on launchd, the parent's `*os.File` on the detached
   start. Never the daemon. That is what makes it survive a panic and a pre-Writer death.
3. **Who bounds it**:
   - POSIX (launchd, detached): the existing `Sweep`/`RotateNow` copy-truncate janitor, with
     `bootLogRingOptions` (2 MB / 1 file). Correct there — the holder is a real append fd.
   - Windows: the shim's size-checked rename (§3.4). Copy-truncate is provably impossible.
   - At install time: `rotateDaemonLogIn`-style pre-launch trim, extended to the boot log, in
     `installLaunchd` and `writeAndCreateWindowsTask` — the gap before the supervisor opens it.
4. **Ordering inside `runDaemonStart`**: the Writer is installed **after** the single-instance
   `flock` is acquired and before the banner. Load-bearing: a second daemon that loses the
   flock race must not rename the live log of the running one. Everything before that point
   (config errors, lock contention) reports to stderr → boot log, which is right.
5. **Run marker**: immediately after installing the Writer, emit
   `log.Printf("[daemon] unarr %s starting (pid %d)", Version, os.Getpid())`. The banner now
   lands in the boot log, so the owned log needs its own unambiguous run delimiter — otherwise
   a rename-rotated `unarr.log` gives no clue where one run ends and the next begins.
6. **Writer-install failure is not fatal**: if `NewWriter` fails (AV lock, read-only dir), log
   one line to stderr and keep logging to stderr. A log file that cannot be opened must not
   stop downloads.

**Does `unarr logs` mention it — yes, in three places:**

- A new `unarr logs --boot` reads the boot ring (`unarr.boot.log`, `.1`) through the same
  `Query`/filter machinery, `MaxFiles: 1`. On a systemd box it returns an explicit error:
  the startup output is in the journal, use `unarr logs`.
- The existing `"no daemon log yet at %s"` error (`logs.go:143`) gains a second sentence when
  a non-empty boot log exists: the daemon wrote startup output there, see `unarr logs --boot`.
  This is the "it never started, and the log directory looks empty" dead end.
- `startDaemonDetached`'s "the daemon exited immediately" error tails the boot log (§3.2).

## 5. `logging.Writer` contract

New file `internal/logging/writer_owned.go`. (`Writer`/`NewWriter` existed in this tree and
were deleted as dead code; this re-specifies them.)

```go
// Writer is an io.Writer that OWNS its log file: it holds the only descriptor and
// rotates by RENAME. Use it only where nothing else holds the file — see RotateNow
// for the copy-truncate rotation that works for a file this process does not own.
type Writer struct {
	mu       sync.Mutex
	path     string
	max      int64 // size budget in bytes; 0 = rotation disabled
	keep     int   // rotated siblings retained
	f        *os.File
	size     int64 // bytes in the live file, tracked in memory
	rotateAt int64 // rotate when size >= rotateAt; 0 = never
	warned   bool  // a rotation failure has already been reported once
}

func NewWriter(opts Options) (*Writer, error)
func (w *Writer) Write(p []byte) (int, error)
func (w *Writer) Close() error
func (w *Writer) Path() string
```

**`NewWriter`**
- `opts.Path == ""` → `errors.New("logging: no log path")`.
- `os.MkdirAll(filepath.Dir(path), logDirMode)`.
- `os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, logFileMode)`. `O_APPEND` is the
  whole point on Windows: it requests `FILE_APPEND_DATA`, a real append handle, so writes
  land at the end regardless of any offset.
- `size` is **seeded from `Stat()`**, not zero. A daemon restart must not hand the file a
  fresh budget — that is how a 20 MB cap becomes 20 MB per restart.
- `max = opts.maxBytes()`, `keep = opts.keep()`, `rotateAt = max`. No rotation is performed in
  the constructor: if the file is already over budget, the first `Write` rotates it. One
  rotation path, not two.

**`Write`**
- Takes `mu` for the whole call. The mutex is the Writer's own guarantee that a write and a
  rotation never interleave — `log.Logger` serializes its own callers, but a second logger or
  a JSON encoder pointed at the same Writer would not.
- If `w.f == nil` (a previous reopen failed), attempt one reopen; on failure return
  `(0, err)`.
- `n, err := w.f.Write(p)`; `w.size += int64(n)`.
- If `err == nil && w.rotateAt > 0 && w.size >= w.rotateAt` → `w.rotateLocked()`.
- **Returns the write's `(n, err)`, never the rotation's.** A failed rotation must not make
  the caller believe the line was lost.

**`rotateLocked`** — returns nothing; it adjusts state and reports at most once.
1. `_ = w.f.Close(); w.f = nil`. **Mandatory before the rename.** Go opens files with
   `sharemode = FILE_SHARE_READ|FILE_SHARE_WRITE` (`syscall_windows.go:395`) — no
   `FILE_SHARE_DELETE` — so Windows refuses to rename a file this process still has open.
2. `err := rotateThroughStaging(w.path, w.keep, renameLive)` — the shared primitive. It
   renames the live file to `w.path + ".rotating"` FIRST and only shifts the ring once that
   rename returned nil. **Superseded step, kept as a record:** this used to call
   `shiftRotated` before the rename, on the argument that "the pre-work is a handful of
   renames, not a 20 MB copy". That argument measures the wrong cost — the cost is not
   syscalls, it is the HISTORY, and a rename blocked by a Windows reader deleted one
   generation of it per budget until all three slots were empty. See §6.
4. **Reopen `w.path` unconditionally**, rename succeeded or not
   (`O_CREATE|O_WRONLY|O_APPEND`). A daemon that stops logging is worse than one that logs
   too much.
5. On reopen success: `w.size = Stat().Size()` — 0 after a successful rename, the unchanged
   old size after a failed one.
6. `rotateAt`:
   - rename succeeded → `w.rotateAt = w.max`;
   - rename failed → `w.rotateAt = w.size + w.max`. **Back off by a full budget.** Without
     this, a permanently blocked rename costs one `shiftRotated` + one `Rename` syscall *per
     log line*, which is a different silent pathology from the one being fixed.
7. On reopen failure: leave `w.f = nil`, `w.size = 0`; the next `Write` retries the open.
8. Reporting: the Writer cannot report through itself. On the first failure only
   (`w.warned`), write one line to `os.Stderr` — which under this design **is** the boot log,
   the correct destination for "the log writer broke".

**Ring slots when the rename fails.** Nothing moves. The ring is only touched after the live
file has been renamed aside, so a blocked rename costs exactly one failed syscall.

*(This paragraph used to read "the ring is shifted by one and slot 1 is empty: one wasted
generation. Accepted." It was not one wasted generation — a permanently blocked rename
repeats every budget, so the whole ring is consumed in `MaxFiles` rotations and the history
is gone while the live file grows unbounded. On Windows the blocker is as ordinary as
leaving `unarr logs -f` open, and the loop cannot break itself: the follower releases the
file only when it observes a rotation. The accepted trade-off was the bug.)*

**`Close`**: take `mu`, close `f` if non-nil, set `f = nil`, return the close error. Idempotent.

**What it does not do**: no time-based rotation, no compression, no external `logrotate`
cooperation. `MaxSizeMB = 0` disables rotation entirely and the Writer degrades to a plain
appending file — the escape hatch for anyone running their own logrotate.

## 6. `RotateNow` / copy-truncate — stays, scoped, and reordered

**It stays.** It is still the only rotation that works for a file this process does **not**
own, and after this change that set is non-empty and permanent:

- `unarr.boot.log` on macOS/launchd and on the detached start;
- `unarr.err.log`, the legacy second macOS file (`daemon_logfiles.go:19`);
- `unarr.log` itself on every install that has not been re-registered yet (§3.3), on
  `unarr logs rotate` against a stopped daemon, and on the pre-launch trim in the installers.

**The ordering defect.** Today it does `shiftRotated` → `snapshot` → `truncate`. On Windows
the first two succeed and the third fails: the ring is destroyed and 20 MB is copied *to
discover afterwards* that the truncate is impossible. Fix: a write probe first.

```go
// probeTruncatable reports whether os.Truncate could work on path, WITHOUT
// touching a byte. On Windows os.Truncate is literally OpenFile(name, O_WRONLY)
// + Ftruncate (os/file_windows.go), so this open is the same open, with the same
// sharing semantics: it fails exactly when a holder like cmd.exe's `>>` redirect
// granted only FILE_SHARE_READ. On POSIX it is a slightly stricter check than
// syscall.Truncate needs — the same write permission, via an open — which costs
// one open/close and never rejects a case truncate would have accepted.
func probeTruncatable(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	return f.Close()
}
```

Final `RotateNow` order: `Path`/`max` guard → `Stat` → under budget? return →
**`Options.Owner` (explicit ownership; refuse with `ErrOwnedByLiveProcess` and touch nothing)**
→ `probeTruncatable` (cheap fail-fast only) → `rotateThroughStaging(path, keep,
copyThenTruncate)`, which copies to `path + ".rotating"`, truncates, and only then shifts the
ring and moves the staging file into slot 1.

No `O_TRUNC`, no `O_APPEND`, no write in the probe. Cost: one open/close per tick.

**The probe is not the safety mechanism, and cannot be.** It answers "would `os.Truncate` be
allowed?", and a Go owner always allows it: Go opens with `FILE_SHARE_WRITE` on Windows and
takes no lock on POSIX. So the probe returns nil in precisely the case that matters — a live
daemon writing the file — which is how `unarr self-update` came to copy-truncate the log of
the daemon it was about to restart. Ownership has to be **declared**: the daemon claims its
`--log-file` in `daemon.state.json` (`agent.ClaimLogFile`), and an external rotator refuses
when that claim names the file AND `isDaemonAlive` says the claimant is still there. A stale
claim (dead PID, or a heartbeat that stopped long ago) must never win, or a crash would block
rotation forever.

**Error message.** The wrapped probe error is what `unarr logs rotate` prints, so it must say
what to do, not just what failed — the running daemon owns this log and rotates it itself,
or another process holds it. That is the difference between the measured message
(`truncate log file: … used by another process`) and an actionable one.

**Janitor.** `Sweep` keeps swallowing errors (a daemon that cannot rotate must keep
downloading) but reports the **first** failure per path through `log.Printf`, once, guarded by
a local bool. The whole 28 GB/day pathology was invisible because nothing ever said a word;
one line per daemon lifetime is the cheapest possible fix for that, and the daemon now owns a
log to say it in.

**`OpenFile`** (`writer.go:63`) stays as-is: it is still how the detached start rotates in the
gap before handing a descriptor to a child. Its target simply becomes the boot log.

## 7. Files to touch

**New**

- `internal/logging/writer_owned.go` — the `Writer` (§5). Separate file from `writer.go` so
  "rotation by rename, we own the fd" and "rotation by copy-truncate, we do not" stay two
  legible, single-purpose units.
- `internal/logging/writer_owned_test.go`
- `internal/logging/janitor_test.go` — the report-once behaviour.
- `internal/cmd/daemon_logsetup.go` — `daemonLogFileFlag`, `bindDaemonLogFlag(cmd)`,
  `installDaemonLogWriter(cfg) (io.Closer, error)`. A new file, not more lines in the
  2369-line `daemon.go`.
- `internal/cmd/daemon_logsetup_test.go`
- `internal/cmd/logs_boot.go` — the `--boot` source selection and the boot-log hint appended
  to the "no daemon log yet" error. Keeps `logs.go` (243 lines, new in Fase 0, therefore
  *not* grandfathered) well under 500.
- `internal/cmd/logs_boot_test.go`
- `internal/cmd/daemon_launch_vbs_test.go` — does not exist today.

**Modified**

- `internal/logging/rotate.go` — `rotateThroughStaging`, the `staged` proof type that owns
  the ring shift, `renameLive`, `copyThenTruncate`, `snapshot`. The single primitive both
  rotation paths share; `shiftRotated` no longer exists as a callable function.
- `internal/logging/owner.go` — `Owner`, `OwnerProbe`, `ErrOwnedByLiveProcess`,
  `Options.refuseIfOwned`.
- `internal/agent/logowner.go` — `ClaimLogFile` / `ReleaseLogFile` / `OwnedLogFile` and
  `DaemonState.LogFile`, stamped by every `WriteState`.
- `internal/cmd/daemon_logowner.go` — `daemonLogOwner`, wired into `logRingOptions`.
- `internal/logging/writer.go` — `probeTruncatable`, the reorder in `RotateNow`, and the doc
  comments: the "UNMEASURED" paragraph at L98-107 is now measured, and the whole comment must
  be rewritten to say *this is for files we do not own* and point at `Writer` for the rest.
- `internal/logging/writer_test.go` — add the probe/ring-intact test; re-frame
  `TestRotateNowUnderANonAppendHolder` around the measurement.
- `internal/logging/janitor.go` — report-once.
- `internal/cmd/daemon_logfiles.go` — `bootLogFileName = "unarr.boot.log"`,
  `bootLogMaxSizeMB = 2`, `bootLogMaxFiles = 1`, `daemonBootLogPath()`,
  `bootLogRingOptions(path)`; `daemonLogPaths()` includes the boot log;
  `startLogJanitors(ctx, every, owned string)` skips a path equal to `owned`.
- `internal/cmd/daemon.go` — three lines: `bindDaemonLogFlag(cmd)` in `newStartCmd`
  (L37-61); the `installDaemonLogWriter` call + run marker after the flock in
  `runDaemonStart`; the new `owned` argument at the `startLogJanitors` call (L362).
  Grandfathered god-file — add nothing else to it.
- `internal/cmd/daemon_launch_vbs.go` — `cmdFor` emits `--log-file` and redirects to the boot
  log; the boot-log size-check-and-rename step at the top of the `Do` loop; doc comment.
- `internal/cmd/daemon_install.go` — `launchdTemplate` (`--log-file` in `ProgramArguments`,
  `Standard*Path` → boot log); `installLaunchd` and `writeAndCreateWindowsTask` also trim the
  boot log before the supervisor opens it; the "log:" lines printed at L351 / L521.
- `internal/cmd/daemon_install_test.go` — plist assertions.
- `internal/cmd/daemon_control.go` (389 lines — must stay under 500) — `startDaemonDetached`
  opens the boot log and passes `--log-file`; the immediate-exit error tails the boot log.
  Extract the choice into a small testable helper rather than testing the fork.
- `internal/cmd/logs.go` — the `--boot` flag on the command + `logsOptions.boot`; route
  through `logs_boot.go`.
- `internal/cmd/logs_test.go`
- `internal/cmd/clean.go` — `unarr.boot.log` and `unarr.boot.log.*` in both `targets`
  (L108-118) and `CleanableBytes` (L386-391). The existing `unarr.log.*` glob does **not**
  match `unarr.boot.log`.
- `internal/cmd/clean_test.go`
- `README.md` — the Logs section (L447-455) and the `[daemon]` config block comments
  (L646-649): describe the two files and which one `log_max_size_mb` governs.

## 8. Tests (each must fail without its change)

| test | file | fails today because |
|---|---|---|
| `TestWriterRotatesByRenameWhileItHoldsTheFile` | `writer_owned_test.go` | `Writer` does not exist |
| `TestWriterSeedsItsSizeFromAnExistingFile` | " | a restart would otherwise get a fresh budget |
| `TestWriterKeepsTheRingBounded` | " | — |
| `TestWriterKeepsWritingWhenRotationFails` | " | read-only dir; asserts writes still succeed and the file still grows |
| `TestWriterBacksOffAfterAFailedRotation` | " | asserts no second rename attempt before another full budget |
| `TestWriterWriteReturnsTheWriteResultNotTheRotationResult` | " | — |
| `TestRotateNowLeavesTheRingIntactWhenItCannotTruncate` | `writer_test.go` | today it shifts + copies 20 MB *before* discovering it cannot truncate. POSIX-implementable with a `0444` log file; guard with `os.Geteuid() != 0` (root ignores mode bits) |
| `TestSweepReportsARotationFailureOnce` | `janitor_test.go` | today it is fully silent |
| `TestStartLogJanitorsSkipsTheLogTheDaemonOwns` | `daemon_logfiles_test.go` | a janitor copy-truncating the owned log would race the Writer and destroy the ring |
| `TestDaemonLogPathsIncludesTheBootLog` | " | — |
| `TestLauncherVBSPassesLogFileAndRedirectsToTheBootLog` | `daemon_launch_vbs_test.go` | asserts `--log-file` present **and** no `>>` at `unarr.log` |
| `TestLauncherVBSRotatesTheBootLogBeforeEachLaunch` | " | — |
| `TestLaunchdPlistOwnsItsLogAndRedirectsBootOutput` | `daemon_install_test.go` | — |
| `TestBuildLogQueryTargetsTheBootRingWithBoot` | `logs_boot_test.go` | — |
| `TestRunLogsPointsAtTheBootLogWhenTheRingIsMissing` | " | — |

Run with `-count=1` (this tree's cache has produced a false failure).

**Windows VM verification is mandatory** (`test/windows/`, the VM is already installed — do
not build another). This design touches `daemon_launch_vbs.go`, `daemon_install*`, and daemon
start/stop/respawn, which is exactly the list `CLAUDE.md` says cross-compilation cannot
prove. Verify, on the real VM: (1) `unarr.log` shrinks after a rename rotation while the
daemon runs; (2) `unarr.boot.log` receives the banner and a forced panic; (3) the shim's boot
log rename fires at the 2 MB budget; (4) `unarr logs` and `unarr logs --boot` both read.

## 9. Risks

1. **Windows: a reader blocks the rename.** Go opens files without `FILE_SHARE_DELETE`, so
   `unarr logs -f` (or an AV scanner) holding `unarr.log` makes `os.Rename` fail. Effect:
   rotation is deferred one budget per attempt and the file overshoots while a tail runs — not
   fatal, and the follower already re-attaches on an inode change (`os.SameFile`, `follow.go`).
   Possible follow-up if it bites: open the follower's handle with `FILE_SHARE_DELETE` via
   `golang.org/x/sys/windows`. Verify on the VM.
2. **The boot log must not be `unarr.log`.** If any implementation keeps `>> unarr.log`
   alongside `--log-file unarr.log`, `cmd.exe`'s `FILE_SHARE_READ` rejects the daemon's
   `FILE_APPEND_DATA` open and the daemon has *no* log. This is the single most important
   invariant in the design.
3. **Mixed old-supervisor / new-binary state.** An upgraded binary under an old plist or old
   shim passes no `--log-file` and behaves exactly as today. Windows self-updates re-register
   the task (`self_update.go:116`), macOS does not — and macOS does not need to, because
   copy-truncate works there. Do not "fix" this into a forced plist rewrite.
4. **`unarr logs rotate` against a running, owning daemon** cannot rename the live file. With
   the probe it now fails fast with an explanatory message instead of shifting the ring and
   copying the file. The message is part of the deliverable, not a nicety.
5. **The banner moves out of `unarr.log`.** Users who grep the log for `unarr Daemon` lose
   that anchor; the `[daemon] unarr X.Y.Z starting (pid N)` run marker replaces it, and it is
   also the only run delimiter left after a rename rotation.
6. **Two writers on one path.** Prevented only by the instance `flock`, which is why the
   Writer must be installed *after* it is acquired. If that ordering is inverted, a second
   daemon that is about to refuse to start will first rename the running daemon's log.
7. **In-memory size counter drift.** The counter tracks what this Writer wrote, seeded from
   `Stat` at open. Ownership is what makes it accurate; if anything else appends, the budget
   is under-counted. Acceptable — and the failure mode is a log slightly over budget, not an
   unbounded one.
8. **Network data dirs (NAS/CIFS/NFS).** A CIFS share can refuse to rename an open file the
   same way Windows does. It lands in the same backoff path, so the failure mode is bounded,
   but a NAS install with the data dir on a share is worth a smoke check.
9. **`MaxSizeMB = 0`.** Rotation off means the Writer never renames and the file grows
   forever — the documented pre-rotation behaviour for users with their own logrotate. An
   external logrotate using `copytruncate` still works against an `O_APPEND` owner on POSIX;
   one using `create` (rename + reopen signal) will silently orphan the daemon's descriptor,
   because the daemon has no `SIGHUP` reopen handler. Out of scope; note it in the README.

---

## Deuda abierta (open debt)

**Status: log rotation is DESCOPED. `[daemon] log_max_size_mb` now defaults to `0`
(`internal/config/config.go:327`), so on a stock install no ring is mutated by anything —
not the daemon's `Writer`, not the janitor, not the installers' pre-launch trim, not
`unarr logs rotate`, not the boot log's ring, not the VBScript shim's trim.** The
architecture stays (the daemon owns `unarr.log` via `--log-file`, the shim redirects startup
output to `unarr.boot.log`); only the rotation is switched off.

Everything below is a residual the audit CONFIRMED in the code that is still there. None of
it is reachable with the default. All of it is reachable the moment a user sets
`log_max_size_mb`. This section exists so that user — and whoever picks the feature back up —
knows exactly what they are accepting.

### a) `commit()` shifts the ring before the FINAL rename is known to work

`internal/logging/rotate.go:68` — `staged.commit()` drops slot N and shifts `.1 → .2 → …`
(`rotate.go:70`) *before* `os.Rename(staging, .1)` at `rotate.go:73`.

`rotateThroughStaging` fixed this one slot to the left: the ring is no longer touched before
the LIVE file operation succeeds. But the invariant stops there. **Failure scenario:** on
Windows a reader holds `unarr.log.1` — and an ordinary `unarr logs -n 200` opens exactly that
file, `internal/logging/reader.go:61` lists every rotated slot as a source. The live rename
into staging succeeds, `.3` is deleted, `.2` is overwritten, `.1 → .2` fails (the reader's
handle, no `FILE_SHARE_DELETE`), and then `staging → .1` also fails. Net result: the oldest
slot is gone, one slot is a duplicate, and the rotated contents are stranded in
`unarr.log.rotating`. The exact class of failure this design was written to remove, one slot
further along.

### b) The intermediate renames are best-effort and unchecked

`internal/logging/rotate.go:70-72` — `_ = os.Rename(...)` per slot, by design ("a slot held by
a reader must not stop a daemon logging").

**Failure scenario:** `.1 → .2` fails while `staging → .1` succeeds. `commit()` returns nil,
every caller treats the rotation as clean, and the contents that were in `.1` have been
silently overwritten by the new slot 1. One rotation, one lost run's worth of history, no
error anywhere. The `keep` count is still honoured, so nothing downstream can notice.

### c) The staging name is shared between processes and nothing locks it

`internal/logging/rotate.go:14` (`stagingSuffix = ".rotating"`) and `rotate.go:39`, where
every rotator computes the same `path + ".rotating"` and starts by `os.Remove`-ing it.

**Failure scenario:** two rotators race — the daemon's `Writer` and an `unarr logs rotate`, or
two `unarr` builds, or a janitor and an installer's pre-launch trim. Rotator A moves the live
file into staging; rotator B, one tick later, sees a live file it can also act on, removes A's
staging file and stages its own. A's `commit()` then shifts the ring for content that no
longer exists and fails at the final rename. The ring advances by two slots and gains nothing.
The `Owner` probe covers daemon-vs-outsider, not outsider-vs-outsider.

### d) `daemonLogOwner` requires a FRESH heartbeat, so a live-but-offline daemon looks unowned

`internal/cmd/daemon_logowner.go:32` delegates to `isDaemonAlive`, and
`internal/cmd/status.go:247` rejects any state whose `LastHeartbeat` is older than 2 minutes.

**Failure scenario:** the daemon process is alive and actively writing `unarr.log` through its
`Writer`, but has been offline for more than two minutes — the server is down, its credentials
were revoked, the NAS lost its uplink. `isDaemonAlive` returns false, `daemonLogOwner` reports
"no owner", and every external rotator (`unarr logs rotate`, the installers' trim, the
`self-update` path) is cleared to copy-truncate a file a live `Writer` owns. That is HIGH-2 —
the bug the ownership claim exists to prevent — reopened through a different door. A liveness
check for *ownership* wants "is this PID still the daemon?", not "is the daemon healthy?", and
those are not the same question.

### e) The boot log's ring has no owner probe at all, and three paths touch it

`internal/cmd/daemon_logfiles.go:66` — `bootLogRingOptions` builds `logging.Options` with no
`Owner` field, unlike `logRingOptions` (`internal/cmd/logs.go:40`), which wires
`daemonLogOwner` in for every caller at once.

Three paths rotate it: `rotateBootLogIn` (`daemon_logfiles.go:91`, from `installLaunchd` and
`writeAndCreateWindowsTask`), the janitor via `logJanitorOptions`
(`daemon_logfiles.go:109`), and `logging.OpenFile` in `startDaemonDetached`
(`internal/cmd/daemon_control.go:316`) — plus the VBScript shim, which is a fourth rotator in
another language entirely. **Failure scenario:** nothing in Go owns this file explicitly, so
the only guard is `probeTruncatable`, which by construction cannot see a Go holder. Two of
those paths running close together (an install while a detached daemon is coming up) shift a
one-slot ring twice and lose the crash evidence the file exists for.

### f) `staged` is package-scoped, so the primitive can be bypassed

`internal/logging/rotate.go:55` — `type staged struct` with an exported-within-package
`commit()`. The doc comment claims "the only way to obtain a `staged` is to get past the op",
and that is true only for code OUTSIDE the package.

**Failure scenario:** any file in `internal/logging` can write
`staged{path: p, staging: s, keep: 3}.commit()` and shift the ring without a live-file
operation ever having run. Nothing — not the type system, not a lint rule — prevents it. The
design's central guarantee rests on a convention, in the one package most likely to grow a
second rotator.

### g) A claim published with a zero heartbeat plus PID reuse

`internal/agent/logowner.go:45` (`ClaimLogFile`) → `logowner.go:64` (`publishLogClaim`), which
writes a state record with `StartedAt` set and `LastHeartbeat` left at its zero value
(`logowner.go:73-78`), deliberately, so the claim exists before registration. `isDaemonAlive`
skips the staleness check on a zero heartbeat (`internal/cmd/status.go:247`) and falls through
to `IsProcessAlive(state.PID)`.

**Failure scenario:** a daemon claims the log, then dies before its first heartbeat (a crash
during startup, a `kill -9`, a power cut). The state file survives with `LastHeartbeat` zero.
The OS reuses that PID for an unrelated process — likelier than it sounds on Windows and on a
long-uptime NAS. `IsProcessAlive` says yes, the zero heartbeat means nothing rejects it, and
the log is now permanently "owned" by a process that has never heard of unarr. Every external
rotation refuses forever, and `unarr logs rotate` tells the user to stop a daemon that is not
running.

### h) Case-folding asymmetry between the janitor's skip and the ownership probe

`internal/cmd/daemon_logfiles.go:128` compares the owned path with
`filepath.Clean(path) == filepath.Clean(owned)` — case-SENSITIVE — while `sameLogFile`
(`internal/cmd/daemon_logowner.go:41-46`) folds case on Windows precisely because the
launcher's spelling and a config-derived one differ in case "surprisingly often".

**Failure scenario:** on Windows the daemon is started with
`--log-file C:\Users\Ana\AppData\Local\unarr\unarr.log` while `config.DataDir()` resolves to
`C:\Users\ana\AppData\Local\Unarr`. `startLogJanitors` does NOT skip the owned file, so a
copy-truncate janitor is started on the very path the `Writer` is renaming. Two rotators on
one file — the exact configuration `startLogJanitors`' doc comment calls "the ring corruption
this whole design exists to remove". The ownership probe, folding case, would have caught it;
the janitor's skip does not.

### i) Older binaries do not honour the claim

The claim is a field in the state file written by `ClaimLogFile`
(`internal/cmd/daemon_logsetup.go:73`). Any unarr binary built before it existed reads the
same state file and ignores the field entirely.

**Failure scenario:** a mixed install — the desktop companion, a second copy under `~/bin`, a
distro package, or `unarr self-update` running the OLD binary to trim the log before swapping
it in — copy-truncates the live log of a daemon that has correctly claimed it. Nothing on the
new side can prevent this; the guard only works when both processes are new enough to speak
it.

### What the descope buys, precisely

With `log_max_size_mb = 0` every one of the above is unreachable, because no code path gets
past its budget check:

| Path | Why it is inert |
|---|---|
| `logging.Writer` (owner's rename) | `Options.maxBytes()` is 0 → `w.rotateAt = 0` → `Write` never calls `rotateIfStillOverLocked` (`internal/logging/writer.go:42`, `writer_owned.go:90`) |
| `logging.RotateNow` | returns nil immediately on `max == 0`, before the `Stat` (`internal/logging/writer.go:132`) |
| `rotateThroughStaging` / `staged.commit()` | only reachable from `Writer.rotateLocked` and `RotateNow`, both stopped above |
| `logging.Sweep` (janitor) | returns before starting its ticker on `maxBytes() == 0` (`internal/logging/janitor.go:28`) |
| `unarr logs rotate` | calls `RotateNow`; prints nothing to do |
| `rotateDaemonLog` / `rotateDaemonLogIn` | call `RotateNow` with `logRingOptions`, whose `MaxSizeMB` is the config value |
| `logging.OpenFile` (boot log, detached start) | calls `RotateNow` before opening; same gate |
| boot log ring (`rotateBootLogIn`, `logJanitorOptions`) | `bootLogRingOptions` returns `MaxSizeMB: 0` unless `rotationEnabled()` (`internal/cmd/daemon_logfiles.go:59,66`) |
| VBScript shim trim | `bootLogTrimBytes()` returns 0 (`internal/cmd/daemon_logfiles.go:81`) and `writeBootLogTrim` emits nothing (`internal/cmd/daemon_launch_vbs.go:242`) |

Pinned by `TestNothingRotatesWithTheDefaultConfig` and
`TestLauncherVBSEmitsNoBootTrimWhenRotationIsOff`
(`internal/cmd/logs_rotation_optin_test.go`), and by `TestLogRotationIsOffByDefault`
(`internal/config/config_rotation_optin_test.go`).

One wart that comes with the shim gate: the Windows boot-log trim is resolved at INSTALL time
and baked into the generated `.vbs`, because nothing inside `wscript.exe` can read unarr's
config. Enabling rotation later therefore needs one `unarr daemon install` before that trim
comes back. Documented in the README's Logs section.
