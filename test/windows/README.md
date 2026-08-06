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

Five checks, each covering something a Linux lab structurally cannot:

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

The bugs it guards: a Windows box that reboots for updates in its 02:00–05:00
maintenance window kills the daemon before its 30s drain can remove the state
file, leaving "running + PID gone" — indistinguishable on disk from a panic, so
the tray mailed a crash report for a restart nobody would call a crash. And the
report it mailed could not have contained a panic anyway, because panics go to
stderr → `unarr.boot.log`, which nothing collected.

Re-run after any change to `internal/sysinfo`, `agent.StateFromPreviousBoot`,
`readStatus`, or `cmd/unarr-desktop/logsources.go`.

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
