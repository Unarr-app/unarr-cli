# Tray Pause/Resume through the supervisor, and one tray at a time - REAL Windows only.
#
# The field report behind this (2026-09-14, windows, v1.11.6): a crash report
# whose unarr.log and unarr.boot.log were eight days older than the daemon that
# died. The tray's Resume ran a bare `unarr start` - a FOREGROUND daemon, the
# tray's own child, no --log-file, its output in an 8 KiB buffer in the tray's
# memory. The tray now runs `unarr daemon start` on Windows, which goes through
# the scheduled task's launcher shim, or - with no task installed - starts a
# detached daemon that still owns its logs. Only a real Windows box can show the
# process tree, the task and the log files that decide whether that holds.
#
# The same report arrived TWICE, 3.7 s apart: two trays watching one daemon.
# Check [3] launches the tray twice and counts what survives.
#
# Run:  powershell -ExecutionPolicy Bypass -File \\host.lan\Data\smoke-resume.ps1
# Writes progress to \\host.lan\Data\resume-result.txt (watch it from the host).
# Needs unarr.exe, unarr-desktop.exe and fakeapi.exe on the share; desktop_test.exe
# too for check [4].
#
# ENCODING: deploy as UTF-8 WITH a BOM and CRLF, and keep non-ASCII out of this
# file (see smoke-supervision.ps1 for the curly-quote failure this avoids).

$ErrorActionPreference = 'Continue'
$Shared  = '\\host.lan\Data'
$Out     = "$Shared\resume-result.txt"
$WorkDir = 'C:\unarr'
$DataDir = "$env:LOCALAPPDATA\unarr"
$CfgDir  = "$env:APPDATA\unarr"
$State   = "$DataDir\daemon.state.json"
$Unarr   = "$WorkDir\unarr.exe"

$script:pass = 0
$script:fail = 0

function Say($msg) {
    $line = "$(Get-Date -Format 'HH:mm:ss')  $msg"
    Write-Host $line
    Add-Content -Path $Out -Value $line -Encoding UTF8
}
function Check($ok, $msg) {
    if ($ok) { $script:pass++; Say "  [PASS] $msg" }
    else     { $script:fail++; Say "  [FAIL] $msg" }
}
function DaemonPids {
    @(Get-Process unarr -ErrorAction SilentlyContinue |
        Where-Object { $_.Path -eq $Unarr } | Select-Object -Expand Id)
}
# Stopwatch, not Get-Date: the guest clock jumps (see smoke-supervision.ps1).
function WaitFor([scriptblock]$cond, [int]$seconds, [string]$what) {
    Say "  waiting up to ${seconds}s for $what ..."
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    while ($sw.Elapsed.TotalSeconds -lt $seconds) {
        if (& $cond) { return $true }
        Start-Sleep -Milliseconds 1000
    }
    return $false
}
function ReadState {
    if (-not (Test-Path $State)) { return $null }
    try { return (Get-Content $State -Raw | ConvertFrom-Json) } catch { return $null }
}
# "Up" means a live unarr.exe that the state file NAMES - after a respawn the file
# still names the previous PID for a few seconds (README gotcha).
function DaemonUpNot([int]$notPid) {
    $d = DaemonPids
    if ($d.Count -eq 0 -or $d[0] -eq $notPid) { return $false }
    $st = ReadState
    return ($st -ne $null -and $st.pid -eq $d[0])
}
function ProcName([int]$procId) {
    $p = Get-CimInstance Win32_Process -Filter "ProcessId=$procId" -ErrorAction SilentlyContinue
    if (-not $p) { return '(gone)' }
    return $p.Name
}
function ParentId([int]$procId) {
    $p = Get-CimInstance Win32_Process -Filter "ProcessId=$procId" -ErrorAction SilentlyContinue
    if (-not $p) { return 0 }
    return [int]$p.ParentProcessId
}
function Evidence($tag) {
    Say "  --- evidence ($tag) ---"
    $st = ReadState
    if ($st) { Say "  state| pid=$($st.pid) status=$($st.status) logFile=$($st.logFile)" } else { Say "  state| (none)" }
    foreach ($f in 'unarr.log', 'unarr.boot.log') {
        if (Test-Path "$DataDir\$f") {
            Say "  $f| modified $((Get-Item "$DataDir\$f").LastWriteTimeUtc.ToString('o'))"
            Get-Content "$DataDir\$f" -Tail 6 -ErrorAction SilentlyContinue | ForEach-Object { Say "  $f| $_" }
        } else { Say "  $f| (missing)" }
    }
    Say "  --- end evidence ---"
}
function StopEverything {
    & $Unarr daemon stop 2>&1 | Out-Null
    Get-Process unarr -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $Unarr } |
        Stop-Process -Force -ErrorAction SilentlyContinue
    Start-Sleep -Seconds 2
    Remove-Item $State, "$DataDir\daemon.stopped" -ErrorAction SilentlyContinue
}

Remove-Item $Out -ErrorAction SilentlyContinue
Say "=== unarr tray resume / single-instance checks on $(([System.Environment]::OSVersion).VersionString) ==="

# -- Setup -----------------------------------------------------------------
Say "[setup] staging binaries + config"
New-Item -ItemType Directory -Force -Path $WorkDir, "$WorkDir\downloads" | Out-Null
Get-Process unarr, unarr-desktop, fakeapi -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
Start-Sleep -Seconds 2
foreach ($b in 'unarr.exe', 'unarr-desktop.exe', 'fakeapi.exe') { Copy-Item "$Shared\$b" $WorkDir -Force }
& $Unarr daemon uninstall 2>&1 | Out-Null
schtasks /delete /tn unarr /f 2>&1 | Out-Null
Remove-Item $State, "$DataDir\daemon.stopped", "$DataDir\desktop.crash-reported" -ErrorAction SilentlyContinue

$wl = Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon' -ErrorAction SilentlyContinue
Say "[info] AutoAdminLogon=$($wl.AutoAdminLogon) DefaultUserName=$($wl.DefaultUserName)"

$env:ADDR = '127.0.0.1:18080'
Start-Process -FilePath "$WorkDir\fakeapi.exe" -WorkingDirectory $WorkDir -WindowStyle Hidden
Start-Sleep -Seconds 2

New-Item -ItemType Directory -Force -Path $CfgDir | Out-Null
$cfg = @(
    '[auth]',
    'api_key = "win-lab-key"',
    'api_url = "http://127.0.0.1:18080"',
    'mirrors = []',
    '',
    '[agent]',
    'id = "win-lab-agent"',
    'name = "win-lab"',
    '',
    '[downloads]',
    'dir = "C:\\unarr\\downloads"',
    'stream_port = 21818',
    'https_stream_port = 21819',
    'auto_https_upnp = false',
    'enable_upnp = false',
    '',
    '[telemetry]',
    'enabled = false',
    '',
    '[library]',
    'auto_scan = false'
)
[System.IO.File]::WriteAllText("$CfgDir\config.toml",
    (($cfg -join "`r`n") + "`r`n"), (New-Object System.Text.UTF8Encoding($false)))

# -- 0. Baseline: what the tray's Resume used to start ----------------------
Say "[0] baseline: a bare 'unarr start' child claims no log file (the bug)"
$bare = Start-Process -FilePath $Unarr -ArgumentList 'start' -WorkingDirectory $WorkDir -PassThru `
    -WindowStyle Hidden -RedirectStandardOutput "$WorkDir\bare-out.txt" -RedirectStandardError "$WorkDir\bare-err.txt"
$bareUp = WaitFor { $st = ReadState; $st -ne $null -and $st.pid -eq $bare.Id } 60 "the bare daemon to register"
Check $bareUp "bare 'unarr start' came up (pid $($bare.Id))"
if ($bareUp) {
    $st = ReadState
    Check ([string]::IsNullOrEmpty($st.logFile)) "bare 'unarr start' state has an EMPTY logFile (logFile='$($st.logFile)')"
} else { Evidence 'bare start' }
Stop-Process -Id $bare.Id -Force -ErrorAction SilentlyContinue
StopEverything

# -- 1. Task installed: Pause / Resume the way the tray now does it ----------
Say "[1] task installed: 'daemon stop' then 'daemon start' keeps the shim, the log and supervision"
& $Unarr daemon install 2>&1 | Out-Null
Start-Sleep -Seconds 3
schtasks /run /tn unarr 2>&1 | Out-Null
$up = WaitFor { DaemonUpNot 0 } 120 "the daemon under the task"
Check $up "daemon up under the scheduled task"
if (-not $up) { Evidence 'task start' }
$pid1 = @(DaemonPids)[0]

& $Unarr daemon stop 2>&1 | Out-Null
$down = WaitFor { (DaemonPids).Count -eq 0 } 20 "Pause to take the daemon down"
Check $down "Pause ('daemon stop') took the daemon down"
Check (Test-Path "$DataDir\daemon.stopped") "Pause recorded the stop intent"
$resurrected = WaitFor { (DaemonPids).Count -gt 0 } 40 "a (wrong) resurrection after Pause"
Check (-not $resurrected) "daemon stayed down after Pause"

$logBefore = (Get-Item "$DataDir\unarr.log" -ErrorAction SilentlyContinue).LastWriteTimeUtc
Start-Sleep -Seconds 2
& $Unarr daemon start 2>&1 | Out-Null
Check ($LASTEXITCODE -eq 0) "Resume ('daemon start') exited 0 (exit $LASTEXITCODE)"
$back = WaitFor { DaemonUpNot $pid1 } 120 "Resume to bring a new daemon up"
Check $back "Resume brought the daemon back"
if ($back) {
    $d = @(DaemonPids)[0]
    $st = ReadState
    Check ($st.logFile -like '*\unarr.log') "resumed daemon CLAIMS its log file (logFile='$($st.logFile)')"
    $parent = ParentId $d
    $pName = ProcName $parent
    $gName = ProcName (ParentId $parent)
    Say "  process tree: unarr.exe($d) <- $pName($parent) <- $gName"
    Check ($pName -eq 'cmd.exe' -and $gName -eq 'wscript.exe') "resumed daemon runs under the launcher shim (cmd.exe <- wscript.exe)"
    # By CONTENT, not LastWriteTime: NTFS may not refresh the directory entry's
    # timestamp while the daemon appends through its open handle (first run of
    # this script: a minute of log lines, an unchanged Get-Item time; the second
    # run did not reproduce it - the [info] line below records which one you got).
    $moved = WaitFor { Select-String -Path "$DataDir\unarr.log" -Pattern "starting \(pid $d\)" -Quiet -ErrorAction SilentlyContinue } 60 "unarr.log to carry the resumed daemon's start line"
    Check $moved "unarr.log is written by the resumed daemon (start line for pid $d)"
    $lagging = (Get-Item "$DataDir\unarr.log").LastWriteTimeUtc -le $logBefore
    Say "  [info] Get-Item LastWriteTime lagging behind the live writer: $lagging"
    Check (-not (Test-Path "$DataDir\daemon.stopped")) "the resumed daemon consumed the stop intent"

    Say "  killing the resumed daemon (pid $d) - the shim must bring it back"
    taskkill /pid $d /f 2>&1 | Out-Null
    $respawn = WaitFor { DaemonUpNot $d } 120 "the shim to relaunch the resumed daemon"
    Check $respawn "a resumed daemon is still SUPERVISED (respawned after a kill)"
    if (-not ($moved -and $respawn)) { Evidence 'after resume' }
} else { Evidence 'resume' }
StopEverything

# -- 1b. Task installed but DISABLED: Resume must not be dead ----------------
# schtasks /query succeeds for a disabled task, so 'daemon start' takes the task
# path, and /run then refuses. It used to stop there with an error - where the old
# bare 'unarr start' Resume did at least start something.
function DaemonStartCli($tag) {
    $p = Start-Process -FilePath $Unarr -ArgumentList 'daemon', 'start' -WorkingDirectory $WorkDir -PassThru `
        -WindowStyle Hidden -RedirectStandardOutput "$WorkDir\$tag-out.txt" -RedirectStandardError "$WorkDir\$tag-err.txt"
    $null = $p.Handle
    $done = $p.WaitForExit(30000)
    Get-Content "$WorkDir\$tag-out.txt", "$WorkDir\$tag-err.txt" -ErrorAction SilentlyContinue |
        Where-Object { $_.Trim() } | Select-Object -First 6 | ForEach-Object { Say "  out| $_" }
    return @{ Done = $done; Code = $p.ExitCode }
}
Say "[1b] task disabled: Pause then Resume falls back to a detached daemon"
& $Unarr daemon install 2>&1 | Out-Null
Start-Sleep -Seconds 3
schtasks /change /tn unarr /disable 2>&1 | Out-Null
# Pause first, exactly as the tray does: that ends the task, so its shim is gone.
& $Unarr daemon stop 2>&1 | Out-Null
$null = WaitFor { (DaemonPids).Count -eq 0 } 20 "Pause to take the daemon down"
$r = DaemonStartCli 'dis'
Check $r.Done "Resume with a disabled task returned"
Check ($r.Code -eq 0) "Resume with a disabled task exited 0 (exit $($r.Code))"
$disUp = WaitFor { DaemonUpNot 0 } 60 "the fallback daemon"
Check $disUp "a daemon is up although the task is disabled"
if ($disUp) {
    $d = @(DaemonPids)[0]
    $st = ReadState
    Check ($st.logFile -like '*\unarr.log') "fallback daemon CLAIMS its log file (logFile='$($st.logFile)')"
    $pName = ProcName (ParentId $d)
    Check ($pName -ne 'cmd.exe') "fallback daemon is detached, not a shim child (parent: $pName)"
} else { Evidence 'disabled task' }
StopEverything

# A disabled task whose shim is STILL running (disabling does not end it) owns a
# daemon already: 'daemon start' has nothing to do and must say so, not fail on
# the boot log the shim's cmd.exe holds open (first run of [1b]: exit 1, "being
# used by another process").
Say "[1c] task disabled while its shim still runs: 'daemon start' is a no-op, exit 0"
schtasks /change /tn unarr /enable 2>&1 | Out-Null
schtasks /run /tn unarr 2>&1 | Out-Null
$null = WaitFor { DaemonUpNot 0 } 120 "the daemon under the task"
$shimPid = @(DaemonPids)[0]
schtasks /change /tn unarr /disable 2>&1 | Out-Null
$r = DaemonStartCli 'run'
Check ($r.Code -eq 0) "'daemon start' against a running disabled task exited 0 (exit $($r.Code))"
$pids = @(DaemonPids)
Check ($pids.Count -eq 1 -and $pids[0] -eq $shimPid) "still exactly the shim's daemon ($($pids -join ','), want $shimPid)"
schtasks /change /tn unarr /enable 2>&1 | Out-Null
StopEverything

# -- 2. No task: Resume must still leave both logs --------------------------
Say "[2] no task installed: 'daemon start' starts a detached daemon that owns its logs"
& $Unarr daemon uninstall 2>&1 | Out-Null
schtasks /query /tn unarr 2>&1 | Out-Null
Check ($LASTEXITCODE -ne 0) "scheduled task is gone"
# NOT Start-Process -Wait: that waits for every DESCENDANT too, and the detached
# daemon is one, so the script hung here forever on its first run.
$cli = Start-Process -FilePath $Unarr -ArgumentList 'daemon', 'start' -WorkingDirectory $WorkDir -PassThru `
    -WindowStyle Hidden -RedirectStandardOutput "$WorkDir\nt-out.txt" -RedirectStandardError "$WorkDir\nt-err.txt"
# Touch Handle at once: Windows PowerShell 5.1 reports an EMPTY ExitCode for a
# -PassThru process whose handle was never opened before it exited (seen here).
$null = $cli.Handle
$exited = $cli.WaitForExit(30000)
Check $exited "'daemon start' without a task returned instead of hanging on its daemon"
Check ($cli.ExitCode -eq 0) "'daemon start' without a task exited 0 (exit $($cli.ExitCode))"
if ($cli.ExitCode -ne 0) {
    Get-Content "$WorkDir\nt-out.txt", "$WorkDir\nt-err.txt" -ErrorAction SilentlyContinue | Select-Object -First 12 | ForEach-Object { Say "  out| $_" }
}
$ntUp = WaitFor { DaemonUpNot 0 } 60 "the detached daemon"
Check $ntUp "detached daemon is up after the CLI exited"
if ($ntUp) {
    $d = @(DaemonPids)[0]
    $st = ReadState
    Check ($st.logFile -like '*\unarr.log') "detached daemon CLAIMS its log file (logFile='$($st.logFile)')"
    Check (Test-Path "$DataDir\unarr.boot.log") "detached daemon has a boot log for panics"
    $pName = ProcName (ParentId $d)
    Say "  parent of detached daemon: $pName"
    Check ($pName -ne 'powershell.exe' -and $pName -ne 'unarr-desktop.exe') "detached daemon is not parented to its launcher"
} else { Evidence 'no-task start' }
StopEverything

# -- 3. One tray at a time ---------------------------------------------------
Say "[3] a second unarr-desktop exits and leaves the first one running"
$env:UNARR_NO_TELEMETRY = '1'
$t1 = Start-Process -FilePath "$WorkDir\unarr-desktop.exe" -WorkingDirectory $WorkDir -PassThru
Start-Sleep -Seconds 6
$t2 = Start-Process -FilePath "$WorkDir\unarr-desktop.exe" -WorkingDirectory $WorkDir -PassThru
$t2gone = WaitFor { $t2.HasExited } 20 "the second tray to exit"
Check $t2gone "the second tray exited on its own"
Check (-not $t1.HasExited) "the first tray is still running"
$trays = @(Get-Process unarr-desktop -ErrorAction SilentlyContinue)
Check ($trays.Count -eq 1) "exactly one unarr-desktop process ($($trays.Count))"
Check (Test-Path "$CfgDir\unarr-desktop.lock") "lock file at $CfgDir\unarr-desktop.lock"
Get-Process unarr-desktop -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
Start-Sleep -Seconds 2
$t3 = Start-Process -FilePath "$WorkDir\unarr-desktop.exe" -WorkingDirectory $WorkDir -PassThru
Start-Sleep -Seconds 6
Check (-not $t3.HasExited) "a tray starts again after the previous one was killed (lock released by the OS)"
Get-Process unarr-desktop -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
StopEverything
# It must not leak into [4]: the crash-report tests assert a report IS sent, and
# with telemetry off handleCrash sends none - five false failures on the first run.
Remove-Item Env:UNARR_NO_TELEMETRY -ErrorAction SilentlyContinue

# -- 4. The Go tests against real Windows ------------------------------------
if (Test-Path "$Shared\desktop_test.exe") {
    Say "[4] desktop package tests for the new code, on Windows"
    Copy-Item "$Shared\desktop_test.exe" $WorkDir -Force
    $pattern = 'TestTrayLock|TestCrashIsReportedOncePerRun|TestNoteCrashSkipsARunAlreadyReported|TestThrottledCrashLeavesNoMark|TestFailedCrashReportIsForgotten|TestReportContext|TestCrashReport|TestSendReport'
    Push-Location $WorkDir
    $res = & "$WorkDir\desktop_test.exe" '-test.v' '-test.run' $pattern 2>&1 | Out-String
    $code = $LASTEXITCODE
    Pop-Location
    $res -split "`n" | Where-Object { $_ -match '^(--- |ok|FAIL|PASS)' } | ForEach-Object { Say "  go| $($_.TrimEnd())" }
    Check ($code -eq 0) "desktop tests pass on Windows (exit $code)"
} else { Say "[4] SKIP: no desktop_test.exe on the share" }

# -- Teardown ----------------------------------------------------------------
Say "[teardown]"
& $Unarr daemon uninstall 2>&1 | Out-Null
Get-Process fakeapi -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue

Say ""
Say "RESULT passed=$($script:pass) failed=$($script:fail)"
Say "DONE"
if ($script:fail -gt 0) { exit 1 } else { exit 0 }
