# Real-Windows test harness for unarr

Cross-compilation (`GOOS=windows go build`) proves the code **builds** for
Windows. It does **not** prove Windows-**runtime** behaviour: that the
`-H=windowsgui` tray and the console daemon spawn children with **no flashing
console window**, or that the scheduled-task autostart actually registers and
survives a logon. Those must be verified on real Windows — this harness is how.

It boots a genuine **Windows 11 VM** (KVM-accelerated QEMU inside a container via
[`dockurr/windows`](https://github.com/dockur/windows)), drops freshly-built
`unarr.exe` + `unarr-desktop.exe` into it, and runs smoke checks.

## Why this exists

The recurring bug class: a GUI-subsystem parent (the tray, built `-H=windowsgui`,
no console) or a windowless daemon spawns a **console-subsystem** child
(`unarr.exe`, `ffmpeg`, `ffprobe`, `powershell`, `cmd`, `taskkill`, `schtasks`…).
Windows then allocates a **new console window** for that child that flashes/stays,
unless the child is spawned with `SysProcAttr{HideWindow:true}` +
`CREATE_NO_WINDOW`. The fix routes every `exec.Command` through
`internal/winproc.HideWindow`; this harness confirms it worked.

Second thing verified here: the Windows autostart (`unarr daemon install`) — a
Task Scheduler task that must carry a logon `<Delay>`, `RestartOnFailure`, and
`StartWhenAvailable` so it doesn't lose the login-time network race (the
"sometimes fails to start at login" complaint).

## Requirements (host)

- `/dev/kvm` present and the user in the `kvm` group (`ls -l /dev/kvm`).
- Docker with `NET_ADMIN` cap allowed (default).
- ~20 GB free for the Windows disk image (cached in a named volume).
- The `dockurr/windows` image: `docker pull dockurr/windows:latest`.

## Usage

```bash
cd test/windows
./run.sh            # build win binaries → boot VM → deploy binaries → print access info
./run.sh --smoke    # same, then wait for WinRM and point you at smoke.ps1
```

- **First boot** downloads + installs Windows 11 **unattended** (~10-20 min).
  The disk image is cached in the `unarr_win_storage` volume, so later boots are
  ~1 min.
- **Watch the desktop:** http://localhost:8006 (noVNC in a browser) or RDP
  `localhost:3389` — user `tester`, password `unarrtest` (test-only VM).
- Built binaries + `CHECKLIST.md` + `smoke.ps1` land on the guest at
  `\\host.lan\Data` (the `./shared/` dir, git-ignored).

### Run the checks (inside the guest)

Open the VM (noVNC/RDP), then in PowerShell:

```powershell
powershell -ExecutionPolicy Bypass \\host.lan\Data\smoke.ps1
```

`smoke.ps1` asserts (exit non-zero on any FAIL):
- `unarr.exe` / `unarr-desktop.exe` run and report a version.
- **No new console window** appears when the tray path spawns a child.
- `unarr daemon install` registers the `unarr` task **with** `<Delay>`,
  `RestartOnFailure`, `StartWhenAvailable`, and **without** the old
  `Start-Transcript -NoClobber`.

`CHECKLIST.md` covers the eyeball-only items a script can't judge (a window
*flashing* during playback/scrub, notifications, tray clicks).

### Fuller E2E — `smoke-full.ps1`

`smoke-full.ps1` exercises real functionality end-to-end: every subcommand's
`--help`, the local commands (`doctor`, `probe-hwaccel` — which spawns real
ffmpeg, `completion`, `config`), authenticated network calls
(`search`/`popular`/`recent`) against the backend, the daemon lifecycle, and —
throughout — asserts the visible console-window count never rises (net delta 0
over the whole run). Bundle `ffmpeg.exe`/`ffprobe.exe` next to the binaries so
the ffmpeg-spawn window check is real; pass a test key via env:

```powershell
$env:UNARR_SMOKE_KEY = '<test api key>'
$env:UNARR_SMOKE_URL = 'https://torrentclaw.com'   # backend for that key
powershell -ExecutionPolicy Bypass \\host.lan\Data\smoke-full.ps1
```

Without `UNARR_SMOKE_KEY` the network checks SKIP; everything else still runs.
Note: `--api-key` is a global flag, but the API URL is **not** a subcommand flag
— it comes from `$env:UNARR_API_URL` (which the script sets from `UNARR_SMOKE_URL`).

### Daemon supervision — `smoke-supervision.ps1`

Verifies what neither cross-compilation nor a Linux lab can: that a killed daemon
comes BACK, and a stopped one stays stopped. Needs `fakeapi.exe` next to the
binaries (`cd test/windows && GOOS=windows go build -o shared/fakeapi.exe ...`
from the lab stub) so the daemon can complete a real registration offline.

```powershell
powershell -ExecutionPolicy Bypass -File \\host.lan\Data\smoke-supervision.ps1
```

**What it established (2026-08-03, Win11 26200):** the scheduled task's
`<RestartOnFailure Count=3 Interval=PT1M>` does **NOT** fire on a non-zero action
exit code. Killing the daemon leaves the task at `Status: Ready, Last Result: 1`
and nothing restarts it — measured with a real logon trigger, not just
`schtasks /run`. That is why supervision lives in the VBScript shim
(`daemon_launch_vbs.go`) as a relaunch loop, and why the exit code alone was not
enough. Do not "simplify" that loop back into a bare `WScript.Quit`.

### Tray Pause/Resume + one tray at a time — `smoke-resume.ps1`

Needs `unarr.exe`, `unarr-desktop.exe`, `fakeapi.exe` and (for check [4])
`desktop_test.exe` on the share:

```bash
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -c -o test/windows/shared/desktop_test.exe ./cmd/unarr-desktop
```

```powershell
powershell -ExecutionPolicy Bypass -File \\host.lan\Data\smoke-resume.ps1
```

**The bug it guards (field crash report 2026-09-14, v1.11.6).** The tray's
Resume ran a bare `unarr start`: a FOREGROUND daemon, the tray's own child, with
no `--log-file` and its output in an 8 KiB buffer in the tray's memory.
`unarr.log` and `unarr.boot.log` stopped moving while the state file stayed
fresh, a panic had nowhere to land, and nothing relaunched it after an
auto-upgrade. The report arrived with logs eight days older than the daemon that
died — and arrived twice, 3.7 s apart, because two trays were watching it.

**What it established (2026-09-14, Win11 26200):**

- [0] baseline: a bare `unarr start` registers with an EMPTY `logFile` — the
  failure mode, reproduced.
- [1] task installed: `daemon stop` → down and stays down; `daemon start` →
  back as `unarr.exe ← cmd.exe ← wscript.exe`, `logFile` claimed, its start line
  in `unarr.log`, and a `taskkill /f` afterwards is respawned by the shim.
- [1b] task installed but DISABLED: `schtasks /query` still succeeds, `/run`
  refuses ("could not run because it is disabled"), and Pause → Resume falls
  back to a detached daemon that claims `unarr.log`.
- [1c] disabled while its shim is STILL running (disabling does not end a task):
  the detached fallback cannot even open `unarr.boot.log` — the shim's
  `cmd /c ... >>` holds it without write sharing ("being used by another
  process"; the first run of [1b] exited 1 on exactly this). That sharing
  violation now means "the shim owns the daemon": exit 0, still one daemon.
- [2] no task: `daemon start` returns (≈1.5 s probe) and leaves a detached daemon
  that claims `unarr.log` and has `unarr.boot.log`, parented to nothing of ours.
- [3] a second `unarr-desktop.exe` exits on its own, exactly one survives, and a
  tray starts again after the previous one was killed (the OS releases the lock).
- [4] the desktop package tests pass on Windows, including the open-handle
  mtime test the crash-report context relies on.

Harness traps this script hit (fixed in it, worth knowing for the next one):
`Start-Process -Wait` waits for DESCENDANTS, so it hangs forever on a command
that leaves a detached daemon — use `-PassThru` + `WaitForExit`; a `-PassThru`
process whose `.Handle` was never touched reports an EMPTY `ExitCode` on 5.1;
and an `$env:` set in one check leaks into the Go tests of a later one
(`UNARR_NO_TELEMETRY=1` made five crash-report tests fail for want of a report).

### What a LOGOFF leaves behind — `smoke-logoff-arm.ps1`

Two phases, because the measurement has to survive the sign-out it measures:
the script starts the daemon (`-Mode shim` or `-Mode detached`), arms a one-shot
`HKCU\...\RunOnce` probe that copies `daemon.state.json` at the next logon (well
inside the task's 20 s logon delay), and signs out. Log back in by hand —
`AutoAdminLogon=1` fires only at boot, not after a logoff, so type the password —
then `Copy-Item C:\unarr\logoff-probe-*.txt \\host.lan\Data\`.

**What it established (2026-09-14, Win11 26200, `-Mode shim`):** the state file
survives the logoff saying `"status": "running"` for a PID that is gone, and
`unarr.log` ends on an ordinary line two seconds before the sign-out — no
shutdown, no `shutting_down`. The shim's daemon does **not** get a usable
`CTRL_LOGOFF_EVENT` despite `cmd /c` giving it a hidden console; the comment in
`daemon.go` ("no console… nothing calls this") holds in practice for the shim
too. Because nothing rebooted, neither `StateFromPreviousBoot` signal (boot
instant, `ShutdownTime`) can tell this state from a crash: the tray started at the
next logon reads it as one. `-Mode detached` was not run (no console at all, so
the same outcome is expected, not measured).

### Log rotation + ownership — `smoke-rotation.ps1`

Verifies the one thing a Linux lab structurally cannot: that the daemon's log
**actually shrinks** while a real `cmd.exe` redirect holder is attached. Needs
`fakeapi.exe` next to the binaries (as `smoke-supervision.ps1` does), otherwise
the daemon does not stay up long enough for check [2] to be conclusive.

```powershell
powershell -ExecutionPolicy Bypass -File \\host.lan\Data\smoke-rotation.ps1
```

**What it established (2026-08-03, Win11 26200) — the bug it now guards.** The
shim used to run `cmd /c ""unarr.exe" start >> "…\unarr.log" 2>&1`, so cmd.exe
held unarr.log with only `FILE_SHARE_READ`. `os.Truncate` on Windows is
`OpenFile(name, O_WRONLY, 0666)` + `Ftruncate`, and that `GENERIC_WRITE` is a
sharing violation, so copy-truncate rotation failed **after** copying the whole
file aside:

```
unarr logs rotate -> exit 1
Error: truncate log file: open C:\unarrlab\unarr\unarr.log:
The process cannot access the file because it is being used by another process.
snapshot unarr.log.1 present = True (2108130 bytes)   <- the copy was made
live unarr.log after the truncate = 2108228 bytes     <- it did not shrink
```

Per 60s janitor tick: the ring shifted (real history gone in three minutes), a
whole budget was copied (~28 GB/day at the 20 MB default), and the live log
never shrank. `janitor.go` swallowed the error, so none of it was visible.

The fix is ownership: the daemon opens unarr.log itself (`start --log-file …`,
O_APPEND ⇒ a real `FILE_APPEND_DATA` handle) and rotates it by **rename**, which
works precisely because the renamer holds the descriptor. cmd.exe keeps only
`unarr.boot.log` — the banner, a fatal start error, a panic dump — which the
shim bounds itself, by rename, at the top of its relaunch loop where nothing
holds the file. The two paths **must stay different files**: point the `>>` at
the log the daemon owns and cmd's `FILE_SHARE_READ` refuses the daemon's own
open, leaving it with no log at all. That is check [1].

Re-run this after ANY change to `daemon_launch_vbs.go`, `daemon_install*`,
`internal/logging`, or the `--log-file` wiring.

### Boot time + crash reports — `smoke-boottime.ps1`

The only check that runs **Go tests inside the guest** rather than driving the
shipped binaries. Deploy the test binaries next to the exes first:

```bash
for p in ./internal/sysinfo ./internal/agent ./cmd/unarr-desktop; do
  GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -c \
    -o "test/windows/shared/$(basename $p | sed s/unarr-desktop/desktop/)_test.exe" "$p"
done
```

```powershell
powershell -ExecutionPolicy Bypass \\host.lan\Data\smoke-boottime.ps1
```

Six checks, each covering something a Linux lab structurally cannot:

1. **`sysinfo.BootTime()` agrees with the OS.** It reads `GetTickCount64`
   through a lazy `kernel32` binding — cross-compiling proves neither that the
   binding resolves nor that the value means what we think. Diffed against
   `Win32_OperatingSystem.LastBootUpTime` (measured 2026-08-04, Win11: 0.5s
   apart). `GetTickCount64` and NOT `QueryUnbiasedInterruptTime`: the unbiased
   clocks stop while the machine sleeps, which on a laptop would place the
   apparent boot *after* a genuine overnight crash and reclassify it as a reboot.
2. **The package tests pass against real Windows syscalls** — `agent` and
   `unarr-desktop`, whose PID-reuse case runs through
   `OpenProcess`/`GetExitCodeProcess` rather than the unix stand-in.
3. Same tests again, **named individually and verbosely**, because the `agent`
   roll-up carries one unrelated failure here (an AST guard that reads its own
   package's `.go` sources, which a `go test -c` binary run from the share does
   not have next to it).
4. **The crash-report log collection against the real CLI** — the tray shells
   out to `unarr daemon logs --boot` to collect the file a Go panic lands in,
   and it SWALLOWS a failure of that call by design (a missing boot log is an
   ordinary state of the world). A CLI answering "unknown flag" would therefore
   restore the original bug in total silence. Needs `unarr.exe` deployed beside
   `desktop_test.exe`; the e2e resolves a CLI from its own directory when there
   is no Go toolchain.
5. **`--boot` really returns the boot log**, checked directly: write a panic
   marker into `unarr.boot.log`, ask the shipped `unarr.exe` for it.
6. **`sysinfo.LastShutdown()` reads and parses the shutdown record.** It pulls a
   FILETIME out of `HKLM\SYSTEM\CurrentControlSet\Control\Windows\ShutdownTime`
   and is diffed against both the raw registry bytes and the shutdown Windows
   itself logged (System event 6006) — a wrong endianness or offset lands
   centuries away, and only a real registry can show that. It also *reports*
   whether the boot instant currently predates the last shutdown, which is the
   Fast Startup blind spot in check 1: a hybrid shutdown hibernates the kernel
   session, so `GetTickCount64` carries over the power cycle and the state file
   of a daemon that shutdown killed looks NEWER than the boot.

The bugs it guards: a Windows box that reboots for updates in its 02:00–05:00
maintenance window kills the daemon before its 30s drain can remove the state
file, leaving "running + PID gone" — indistinguishable on disk from a panic, so
the tray mailed a crash report for a restart nobody would call a crash. And the
report it mailed could not have contained a panic anyway, because panics go to
stderr → `unarr.boot.log`, which nothing collected.

Then the same bug's second half, from a crash report filed 2026-08-26: a box
shut down at 00:02 with **Fast Startup** on. The boot-time filter above cannot
see that power cycle at all (hibernated kernel session ⇒ the tick counter
carries over), so the shutdown record became the second, independent signal —
and the daemon now stamps `shutting_down` **before** its drain, so any stop that
outruns the drain stops looking like a death in the first place. Note the daemon
gets no shutdown signal on Windows regardless (`DETACHED_PROCESS |
CREATE_NO_WINDOW` ⇒ no console ⇒ no `CTRL_SHUTDOWN_EVENT`), which is exactly why
the after-the-fact verdict has to carry that case.

Re-run after any change to `internal/sysinfo`, `agent.StateFromPreviousBoot`,
`readStatus`, or `cmd/unarr-desktop/logsources.go`.

To exercise check 6 against a REAL power cycle rather than whatever the box last
did: note the reported `lastShutdown`, then `Stop-Computer` in the guest, boot it
again (`docker compose up -d`) and re-run — the value must move forward to the
shutdown you just performed. **Measured 2026-08-26:** it does, and
`sysinfo.LastShutdown()` parsed it to within 0.4s of event 6006.

**Known non-finding — `desktop_test.exe` roll-up (check 2) fails in this guest.**
`TestReportLogsWithoutABootLog` and `TestReportLogsPlaceholderSurvives` assert
what the tray shows when there is NO log to collect, and this guest always has
one: check 5 writes a panic marker into `%LOCALAPPDATA%\unarr\unarr.boot.log`,
so from the second run onward those fixtures cannot hold. Verified 2026-08-26 to
fail identically with a `desktop_test.exe` built from `main`, i.e. it is the
harness contaminating itself, not a regression. The checks that matter are the
per-name ones (check 3), which must stay all-green. Delete that boot log if you
want the roll-up green.

**What this harness cannot show you:** the Fast Startup carry-over itself. The
guest reports `HiberbootEnabled=1`, but its firmware has no hibernation
(`powercfg /a` → "Fast Startup — Hibernation is not available"), so every
`Stop-Computer` here is a FULL shutdown and the tick counter always restarts —
check 6 will keep printing "boot is after the last shutdown". Reproducing the
blind spot needs real hardware (or a VM with hibernation), which is why the
shutdown record is written to be a no-op when it has nothing to say.

### Piece-completion DB quarantine — `smoke-piece-completion.ps1`

Runs the `internal/engine` tests for the bolt piece-completion backend and its
pre-flight (`piece_completion_bolt.go`, `piece_completion_check.go`,
`torrent_storage.go`) on real Windows. Deploy the package test binary first.
Build it `CGO_ENABLED=0` like the release (the backend itself is ours and no
longer depends on cgo, but the `InjectedDamage` fixture still goes through the
library's legacy backend, and the release is what you want to match anyway):

```bash
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -c -o test/windows/shared/engine_test.exe ./internal/engine
```

```powershell
powershell -ExecutionPolicy Bypass -File \\host.lan\Data\smoke-piece-completion.ps1
$env:UNARR_ENGINE_RUN = '.'              # optional: the whole engine package instead of the quarantine tests
$env:UNARR_TEST_BIN = 'cmd_test.exe'     # optional: another package's test binary from the share
$env:UNARR_ENGINE_RUN = 'Clean'          #   (e.g. the `unarr clean` targets) — result in cmd-result.txt
```

Result lands in `shared/engine-result.txt` (`EXIT=0` + one `--- PASS` per test;
the script deletes the previous result and the previous binary first, so a stale
pass cannot survive a failed copy). `$env:` values persist for the whole
PowerShell session — after a `'.'` run, `Remove-Item Env:UNARR_ENGINE_RUN` or the
next "targeted" run is silently the whole package again.

**What only this can prove:** bbolt locks with `LockFileEx` here, the check runs
in a re-exec'd CHILD of the test binary (`UNARR_BOLT_CHECK` env, the way
`cmd/unarr/main.go` does it — `CheckerCrashCountsAsCorrupt` kills that child on
purpose and needs Windows process exit codes to come back right), and the
quarantine renames a DB it has just closed over an existing `.corrupt` file —
the `LockedIsLeftAlone` case (a second daemon holding the file must make the
check step aside, never move the file), the child-exit mapping and the
replace-on-rename are Windows semantics a Linux run cannot stand in for.
The salvage path (`ReachableFreedPageIsSalvaged`: `bbolt.Compact` in the child,
two renames in the parent) is also exercised here on NTFS. Measured 2026-09-07
(Win11 26200): 19/19 targeted (1 POSIX-only skip), whole package green, plus
`cmd_test.exe -test.run Clean` for the `unarr clean` targets.

### Doctor / support-bundle package tests — `smoke-doctorwin.ps1`

Deploy the package test binaries first:

```bash
for p in ./internal/cmd ./internal/support; do
  GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -c -o "test/windows/shared/$(basename $p)_test.exe" "$p"
done
```

```powershell
powershell -ExecutionPolicy Bypass \\host.lan\Data\smoke-doctorwin.ps1
```

**What only this can prove.** It caught a real bug in `doctor`'s port check
that Linux cannot expose: `net.Listen` on `:P` (wildcard) **succeeds on
Windows while another process holds `127.0.0.1:P`** — the two are not a
conflict without `SO_EXCLUSIVEADDRUSE`. A wildcard-only test therefore
reported "port is free" for a port that was demonstrably taken, which is the
worst answer that check can give: the reassuring one. `portIsFree` now binds
both and counts the port free only if both succeed.

Also settles the home-path scrubbing against a real `C:\Users\…`, where the
separator and the case-insensitive filesystem both differ from the unix
assumptions.

Re-run after any change to `internal/support`'s scrubber or the doctor port
checks. Note the script SKIPS the AST-guard tests (`stopintent_wiring_test.go`
and friends): they read their own package's `.go` sources, which do not travel
next to a `go test -c` binary. Those failures are the harness, not findings —
letting them show red trains the reader to ignore red.

### Gotchas that cost real time here (read before writing a .ps1)

- **Copy binaries to a LOCAL directory before running them.** A process started
  from `\\host.lan\Data` inherits a **UNC working directory**, and every child
  it spawns then fails — measured as `exit status 1`, **zero bytes** of output
  and **~72s of SMB timeout per call**, even for `unarr version`, which touches
  no files. The failure looks exactly like a broken product (the e2e reported
  "No logs available" and took 296s) and is nothing of the kind: copy
  `unarr.exe` + the test binary to e.g. `C:\unarrtest` and the same run takes
  **1.1s**. Note the CLI must land in the SAME directory as the test binary,
  which resolves it as a sibling when there is no Go toolchain. Running a .ps1
  off the share is fine; only spawning processes from a UNC cwd is not.
- **Deploy .ps1 as UTF-8 WITH a BOM and CRLF.** Windows PowerShell 5.1 decodes a
  BOM-less file as CP1252, which turns the UTF-8 bytes of an em dash into a CURLY
  QUOTE — and PowerShell accepts curly quotes as string delimiters. One em dash
  inside a double-quoted string silently ends it, every quote after it pairs up
  wrong, and the parse error surfaces on the LAST line of the file, pointing
  nowhere near the cause. Keep non-ASCII out of quoted strings too.
  `{ printf '\xef\xbb\xbf'; sed 's/$/\r/' x.ps1; } > shared/x.ps1`
- **Quote every `-test.*` flag when running a `go test -c` binary from PowerShell.**
  PS 5.1 tokenises a bare `-test.v` as `-test` + `.v`, and the binary answers
  `flag provided but not defined: -test` followed by its whole usage text. Write
  `& $exe '-test.v' '-test.run' $pattern` (see `smoke-piece-completion.ps1`).
- **No here-strings.** PS 5.1 fails to find the `"@` terminator in an LF file and
  swallows the rest of the script. Build multi-line text as an array and join it.
- **Write config files without a BOM.** `Set-Content -Encoding UTF8` on 5.1 adds
  one; three stray bytes in front of the first TOML key make the config parse as
  empty and the daemon exits with "no API key configured" — which reads exactly
  like a startup crash. Use `[System.IO.File]::WriteAllText(path, text,
  (New-Object System.Text.UTF8Encoding($false)))`.
- **Never time anything with `Get-Date` deltas.** The guest clock resyncs against
  the host and jumps backwards by hours; a `(Get-Date).AddSeconds(n)` deadline
  then never arrives and the run hangs. Use
  `[System.Diagnostics.Stopwatch]::StartNew()`.
- **"Is the daemon up?" is not "does the state file exist?"** The state file is
  written during registration, seconds into startup, and survives a crash — so
  after a respawn it still names the PREVIOUS, dead PID. Match the PID:
  `(Get-Content state.json -Raw) -match "`"pid`":\s*$want"`. Asserting mere
  existence produces failures that look like product bugs and are not.
- **A firewall prompt steals focus** the first time a binary listens, and then
  `qtype.py` keystrokes go to the dialog instead of your shell. Pre-authorise:
  `netsh advfirewall firewall add rule name=unarr dir=in action=allow
  program=C:\unarr\unarr.exe enable=yes`. `sendkey esc` dismisses one.
- **WinRM is usually NOT up** on an already-installed VM (it is enabled only by
  `oem/install.bat`, which runs on a fresh install). Do not wait on port 5985 —
  use the `qtype.py` path below.
- **Screenshots without vncdotool:** `python3 qmon.py 'screendump /tmp/s.ppm'`
  against the monitor socket, then `docker cp` + `convert` to PNG. `qmon.py` also
  sends any other monitor command (`sendkey`, `info status`).

### Driving the guest headlessly (no WinRM)

WinRM does not auto-enable in `dockurr/windows`, and **vncdotool `type` does not
reach the guest** on this QEMU/Windows — but the **QEMU monitor `sendkey`** does.
The reliable path used here:
- Screenshot: run `vncdotool ... capture` against the container-internal VNC
  `:5900` (install vncdotool into the container's python3), then `docker cp` out.
- Type: `scripts/qtype.py`-style helper writing `sendkey` lines to the QEMU
  monitor unix socket `/run/shm/monitor.sock`. Open PowerShell with
  `sendkey meta_l` → type `powershell` → `sendkey ret`.
- Run a `.ps1`: `Set-ExecutionPolicy -Scope Process Bypass -Force` first.
- Result files come out **UTF-16** → decode with `iconv -f UTF-16 -t UTF-8`.
The `oem/install.bat` auto-path only fires on a FRESH install (`down -v`).

## Lifecycle

```bash
docker compose ps                 # state
docker compose logs -f            # install / boot progress
docker compose down               # stop VM, KEEP the disk volume (fast re-boot next time)
docker compose down -v            # full reset — DELETES the Windows install
```

## Notes / gotchas

- **KVM required.** Without `/dev/kvm` the VM falls back to TCG (software) and is
  unusably slow — check `ls -l /dev/kvm` and group membership first.
- **First boot is slow** and pulls Windows from Microsoft's servers; a flaky
  network can stall the download (watch `docker compose logs -f`).
- `./shared/` is git-ignored — it holds the built `.exe`s per run, not source.
- This is a **test-only** VM with a throwaway password; never connect it to real
  accounts or credentials.
- The image tag is pinned to `latest` here for convenience; pin a digest if a
  `dockurr/windows` update ever changes the unattended-install behaviour.
- Bump the binaries and re-run `./run.sh` after any change to Windows spawn code
  (`internal/winproc`, `detach_windows.go`, `daemon_install*`, any new
  `exec.Command`). The `internal/winproc` AST guard test catches missing
  `HideWindow` calls at `go test` time — this harness confirms the runtime effect.
```
