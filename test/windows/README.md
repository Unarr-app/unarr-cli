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
