# Daemon supervision checks — REAL Windows only.
#
# These verify the one thing neither cross-compilation nor a Linux lab can: that
# Task Scheduler actually brings the daemon back after it dies, and leaves it
# down after the user stops it. The mechanism spans three processes — the task,
# wscript.exe running the VBScript shim, and unarr.exe — and the signal between
# them is the shim's EXIT CODE. A shim that falls off the end exits 0, Task
# Scheduler reads "succeeded", RestartOnFailure never fires, and a daemon that
# died seconds after logon stays dead until the next logon.
#
# Run:  powershell -ExecutionPolicy Bypass -File \\host.lan\Data\smoke-supervision.ps1
# Writes progress to \\host.lan\Data\supervision-result.txt (watch it from the host).
#
# ENCODING: deploy this file to the guest as UTF-8 WITH a BOM and CRLF endings.
# Windows PowerShell 5.1 decodes a BOM-less .ps1 as CP1252, which turns the UTF-8
# bytes of an em dash into a CURLY QUOTE - and PowerShell accepts curly quotes as
# string delimiters, so one em dash inside a double-quoted string silently ends
# that string and every quote after it pairs up wrong. The parse error then
# surfaces on the LAST line of the file, pointing nowhere near the cause. Keep
# non-ASCII out of quoted strings here regardless; the BOM is the belt.

$ErrorActionPreference = 'Continue'
$Shared  = '\\host.lan\Data'
$Out     = "$Shared\supervision-result.txt"
$WorkDir = 'C:\unarr'
$DataDir = "$env:LOCALAPPDATA\unarr"
$CfgDir  = "$env:APPDATA\unarr"

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
        Where-Object { $_.Path -eq "$WorkDir\unarr.exe" } | Select-Object -Expand Id)
}
# Wait until `cond` is true or the timeout expires. Returns whether it became true.
#
# Uses a Stopwatch, NOT wall-clock arithmetic: this VM's guest clock resyncs
# against the host and can jump BACKWARDS by hours, which left a Get-Date
# deadline unreachable and hung the run. A monotonic timer cannot be fooled that
# way. Polls fast (1s) because a daemon that fails at startup can come and go
# between two slow polls and look like it never ran.
function WaitFor([scriptblock]$cond, [int]$seconds, [string]$what) {
    Say "  waiting up to ${seconds}s for $what ..."
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    while ($sw.Elapsed.TotalSeconds -lt $seconds) {
        if (& $cond) { return $true }
        Start-Sleep -Milliseconds 1000
    }
    return $false
}
# Dump whatever the daemon and Task Scheduler have to say. Called whenever
# something fails, so a red line always arrives with its evidence attached.
function Evidence($tag) {
    Say "  --- evidence ($tag) ---"
    if (Test-Path "$DataDir\unarr.log") {
        Get-Content "$DataDir\unarr.log" -Tail 15 -ErrorAction SilentlyContinue |
            ForEach-Object { Say "  log| $_" }
    } else { Say "  log| (no $DataDir\unarr.log)" }
    $q = schtasks /query /tn unarr /fo LIST /v 2>&1 | Out-String
    foreach ($k in 'Status','Last Run Time','Last Result','Next Run Time','Scheduled Task State') {
        $m = ($q -split "`n" | Where-Object { $_ -match "^\s*$k\s*:" } | Select-Object -First 1)
        if ($m) { Say "  task| $($m.Trim())" }
    }
    Say "  --- end evidence ---"
}

Remove-Item $Out -ErrorAction SilentlyContinue
Say "=== unarr daemon supervision checks on $(([System.Environment]::OSVersion).VersionString) ==="

# ── Setup ───────────────────────────────────────────────────────────────────
Say "[setup] staging binaries + config"
New-Item -ItemType Directory -Force -Path $WorkDir | Out-Null
Copy-Item "$Shared\unarr.exe"   $WorkDir -Force
Copy-Item "$Shared\fakeapi.exe" $WorkDir -Force

# Clean slate: no leftover task, state file or stop marker from an earlier run.
& "$WorkDir\unarr.exe" daemon uninstall 2>&1 | Out-Null
schtasks /delete /tn unarr /f 2>&1 | Out-Null
Get-Process unarr, fakeapi -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
Start-Sleep -Seconds 2
Remove-Item "$DataDir\daemon.state.json", "$DataDir\daemon.stopped" -ErrorAction SilentlyContinue

# A stub backend so the daemon can complete a real registration offline.
# -Environment is PowerShell 7+; Windows PowerShell 5.1 inherits the caller's env.
$env:ADDR = '127.0.0.1:18080'
Start-Process -FilePath "$WorkDir\fakeapi.exe" -WindowStyle Hidden
Start-Sleep -Seconds 2

# Config built as an array, NOT a here-string: Windows PowerShell 5.1 fails to
# find the `"@` terminator in an LF-only file and swallows the rest of the script
# as one unterminated string (seen here as a parse error on the very last line).
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
# Written WITHOUT a BOM on purpose. `Set-Content -Encoding UTF8` in Windows
# PowerShell 5.1 emits UTF-8 WITH a BOM, and those three bytes land in front of
# the first TOML key - the config then parses as empty and the daemon exits
# immediately with "no API key configured", which looks exactly like a daemon
# that crashed on startup.
[System.IO.File]::WriteAllText("$CfgDir\config.toml",
    (($cfg -join "`r`n") + "`r`n"), (New-Object System.Text.UTF8Encoding($false)))
$firstBytes = [System.IO.File]::ReadAllBytes("$CfgDir\config.toml")[0..2]
Check ($firstBytes[0] -ne 0xEF) "config.toml written without a BOM"

# The daemon must actually STAY up, or every check after this is meaningless.
Say "[setup] verifying the daemon can start at all"
$probe = Start-Process -FilePath "$WorkDir\unarr.exe" -ArgumentList 'start' -PassThru `
    -WindowStyle Hidden -RedirectStandardOutput "$Shared\probe-out.txt" `
    -RedirectStandardError "$Shared\probe-err.txt"
Start-Sleep -Seconds 8
if ($probe.HasExited) {
    Check $false "daemon stays up when started by hand (exit code $($probe.ExitCode))"
    Get-Content "$Shared\probe-out.txt", "$Shared\probe-err.txt" -ErrorAction SilentlyContinue |
        Select-Object -First 12 | ForEach-Object { Say "  out| $_" }
    Evidence 'manual start'
    Say "RESULT passed=$($script:pass) failed=$($script:fail)"
    Say "DONE"
    exit 1
}
Check $true "daemon stays up when started by hand"
Stop-Process -Id $probe.Id -Force -ErrorAction SilentlyContinue
Start-Sleep -Seconds 3
Remove-Item "$DataDir\daemon.state.json", "$DataDir\daemon.stopped" -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path "$WorkDir\downloads" | Out-Null

# ── 1. The shim must carry an exit code ─────────────────────────────────────
Say "[1] daemon install writes a launcher that reports failure to Task Scheduler"
& "$WorkDir\unarr.exe" daemon install 2>&1 | Out-Null
Start-Sleep -Seconds 5

$vbsPath = "$DataDir\unarr-launch.vbs"
Check (Test-Path $vbsPath) "launcher shim written to $vbsPath"
if (Test-Path $vbsPath) {
    # The shim is UTF-16LE+BOM on disk — that is what Windows Script Host needs.
    $bytes = [System.IO.File]::ReadAllBytes($vbsPath)
    Check ($bytes[0] -eq 0xFF -and $bytes[1] -eq 0xFE) "shim is UTF-16LE with a BOM (WSH decodes it correctly)"
    $vbs = [System.IO.File]::ReadAllText($vbsPath, [System.Text.Encoding]::Unicode)
    Check ($vbs -match 'WScript\.Quit')   "shim sets an explicit exit code"
    Check ($vbs -match 'WScript\.Quit 1') "shim reports FAILURE when the stop was not requested"
    Check ($vbs -match 'WScript\.Quit 0') "shim reports success when the stop WAS requested"
    Check ($vbs -match 'daemon\.stopped') "shim consults the stop-intent marker"
}
$xml = schtasks /query /tn unarr /xml 2>&1 | Out-String
Check ($xml -match 'RestartOnFailure')      "task carries RestartOnFailure"
Check ($xml -match 'wscript')               "task action launches the shim via wscript"

# ── 2. A crash must bring the daemon back ───────────────────────────────────
Say "[2] a killed daemon is respawned by the scheduled task"
schtasks /run /tn unarr 2>&1 | Out-Null
$up = WaitFor { (DaemonPids).Count -gt 0 } 120 "the daemon to come up"
Check $up "daemon started under the scheduled task"

if (-not $up) { Evidence 'daemon never came up under the task' }

if ($up) {
    $before = DaemonPids
    Say "  daemon PID(s): $($before -join ',')"
    # The state file is written during register(), several seconds into startup —
    # asserting the instant the process appears is a race, not a check.
    $hasState = WaitFor { Test-Path "$DataDir\daemon.state.json" } 60 "the daemon to register"
    Check $hasState "state file present while running"
    if (-not $hasState) { Evidence 'no state file after startup' }

    # Kill it the way an AV / a hard fault would: no chance to clean up.
    Say "  killing PID $($before[0]) with taskkill /f (simulating the crash)"
    taskkill /pid $before[0] /f 2>&1 | Out-Null

    Check (-not (Test-Path "$DataDir\daemon.stopped")) "a crash leaves NO stop-intent marker"
    Start-Sleep -Seconds 3
    Check (Test-Path "$DataDir\daemon.state.json") "state file SURVIVES a crash (so the tray still reports it)"

    # RestartOnFailure interval is PT1M; allow generous slack for the task engine.
    $back = WaitFor { $now = DaemonPids; $now.Count -gt 0 -and $now[0] -ne $before[0] } 240 "the task to respawn the daemon"
    Check $back "daemon RESPAWNED after the crash"
    if ($back) { Say "  new daemon PID(s): $((DaemonPids) -join ',')" }
    else       { Evidence 'no respawn' }
}

# ── 3. A requested stop must stay stopped ───────────────────────────────────
Say "[3] a deliberate stop is honoured (no resurrection)"
& "$WorkDir\unarr.exe" stop 2>&1 | Out-Null
Start-Sleep -Seconds 5

Check (Test-Path "$DataDir\daemon.stopped")        "stop recorded the intent marker"
Check (-not (Test-Path "$DataDir\daemon.state.json")) "clean stop left NO state file (no false crash report)"
Check ((DaemonPids).Count -eq 0)                   "daemon is down after stop"

# If the shim mis-reported, the task restarts it about a minute later.
$resurrected = WaitFor { (DaemonPids).Count -gt 0 } 150 "a (wrong) resurrection"
Check (-not $resurrected) "daemon STAYED down - the pause was not undone by the supervisor"

# ── Teardown ────────────────────────────────────────────────────────────────
Say "[teardown]"
& "$WorkDir\unarr.exe" daemon uninstall 2>&1 | Out-Null
Get-Process fakeapi -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue

Say ""
Say "RESULT passed=$($script:pass) failed=$($script:fail)"
Say "DONE"
if ($script:fail -gt 0) { exit 1 } else { exit 0 }
