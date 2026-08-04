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

### Gotchas that cost real time here (read before writing a .ps1)

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
