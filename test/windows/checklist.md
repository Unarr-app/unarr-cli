# unarr — manual Windows verification checklist

Run this against the test VM (`./run.sh`, then open http://localhost:8006 or RDP
`localhost:3389`, user `tester` / `unarrtest`). The built binaries are on the
guest at `\\host.lan\Data` (`unarr.exe`, `unarr-desktop.exe`).

Most of this is automated by `smoke.ps1` — run that first:
`powershell -ExecutionPolicy Bypass \\host.lan\Data\smoke.ps1`. The eyeball items
below are the ones a script can't judge (a window *flashing*).

## Console-window suppression (the reported bug)

- [ ] **Daemon start shows NO console window.** Copy `unarr.exe` locally, run
      `unarr daemon install` then reboot / log off+on. On login the agent starts
      and **no black terminal window appears** (previously one popped up).
- [ ] **Tray control clicks show no flash.** Launch `unarr-desktop.exe`. Click
      Pause, then Resume, then Restart in the tray. **No console window flashes**
      on any of them (controlwatch.go spawn path).
- [ ] **Notifications show no flash.** Trigger a notification (finish/fail a
      download). **No PowerShell window flashes** (notify.go).
- [ ] **Open downloads / View logs show no flash.** Tray → Open downloads folder,
      and → View logs. Explorer/editor opens with **no cmd.exe blink** (agentctl
      `cmd /c start`).
- [ ] **Library scan / playback show no flashes.** Play something that triggers
      ffprobe/ffmpeg (thumbnails on hover, transcode). Scrub the timeline. **No
      swarm of console windows** (the ~34 media-tool exec sites).

## Autostart reliability (the "sometimes fails to start at login" complaint)

- [ ] `unarr daemon install` → `schtasks /query /tn unarr /xml` shows
      `<Delay>PT20S</Delay>`, `<RestartOnFailure>`, `<StartWhenAvailable>true`.
- [ ] Log off and back on: the agent is running within ~30s **without** manual
      intervention (previously it sometimes stayed dead until Resume).
- [ ] Kill the daemon process; within ~1 min the task restarts it (RestartOnFailure).

## Regression / sanity

- [ ] `unarr --version` and `unarr-desktop --version` both print a version.
- [ ] Player launch (mpv/VLC/system default) still opens and plays.
- [ ] `unarr daemon uninstall` removes the task cleanly (`schtasks /query` → not found).

Record PASS/FAIL here and paste into the PR/commit notes.
