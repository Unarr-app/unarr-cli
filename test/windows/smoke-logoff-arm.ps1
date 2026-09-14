# What a LOGOFF leaves in the daemon state file - REAL Windows only, two phases.
#
# The question: when the user signs out (or Windows signs them out for an update),
# does the daemon get to record "shutting_down" before it dies? The code comments
# long assumed it never can on Windows ("no console, no CTRL_SHUTDOWN_EVENT"),
# which is true for a DETACHED daemon but not obviously for the launcher shim's,
# whose `cmd /c` gives it a hidden console. If a logoff leaves a bare "running",
# the tray started at the next logon sees a dead PID and mails a crash report for
# a sign-out - and no boot-time filter can catch it, because nothing rebooted.
#
# Phase 1 (this script): start the daemon in the chosen mode, arm a one-shot
# HKCU RunOnce probe (no elevation needed) that copies the state file at the next
# logon, then sign out. The probe runs within seconds of logon, well inside the
# scheduled task's 20 s logon delay, so the relaunched daemon cannot overwrite
# the evidence first.
# Phase 2 (after logging back in): Copy-Item C:\unarr\logoff-probe-*.txt \\host.lan\Data\
#
# AutoAdminLogon=1 does NOT sign back in after a logoff - it only fires at boot.
# The guest sits at the password prompt: send `ret` (dismiss), type the password
# (qtype.py 'unarrtest'), `ret`. Any keystroke sent before that - a Win+R, a
# command line - lands in the password box and costs a failed attempt (seen).
#
# Run:  powershell -ExecutionPolicy Bypass -File \\host.lan\Data\smoke-logoff-arm.ps1 -Mode shim
#       powershell -ExecutionPolicy Bypass -File \\host.lan\Data\smoke-logoff-arm.ps1 -Mode detached
# Assumes smoke-resume.ps1 ran first on this guest (binaries in C:\unarr, config).
#
# ENCODING: deploy as UTF-8 WITH a BOM and CRLF; keep this file ASCII.

param([string]$Mode = 'shim')

$ErrorActionPreference = 'Continue'
$Shared  = '\\host.lan\Data'
$Out     = "$Shared\logoff-arm-$Mode.txt"
$WorkDir = 'C:\unarr'
$DataDir = "$env:LOCALAPPDATA\unarr"
$State   = "$DataDir\daemon.state.json"
$Unarr   = "$WorkDir\unarr.exe"

function Say($msg) {
    $line = "$(Get-Date -Format 'HH:mm:ss')  $msg"
    Write-Host $line
    Add-Content -Path $Out -Value $line -Encoding UTF8
}
function DaemonPids {
    @(Get-Process unarr -ErrorAction SilentlyContinue |
        Where-Object { $_.Path -eq $Unarr } | Select-Object -Expand Id)
}
function WaitFor([scriptblock]$cond, [int]$seconds) {
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    while ($sw.Elapsed.TotalSeconds -lt $seconds) {
        if (& $cond) { return $true }
        Start-Sleep -Milliseconds 1000
    }
    return $false
}

Remove-Item $Out -ErrorAction SilentlyContinue
Say "=== logoff probe, mode=$Mode ==="

& $Unarr daemon stop 2>&1 | Out-Null
Get-Process unarr -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $Unarr } | Stop-Process -Force -ErrorAction SilentlyContinue
Start-Sleep -Seconds 2
Remove-Item $State, "$DataDir\daemon.stopped", "$WorkDir\logoff-probe-$Mode.txt" -ErrorAction SilentlyContinue

if (-not (Get-Process fakeapi -ErrorAction SilentlyContinue)) {
    $env:ADDR = '127.0.0.1:18080'
    Start-Process -FilePath "$WorkDir\fakeapi.exe" -WorkingDirectory $WorkDir -WindowStyle Hidden
    Start-Sleep -Seconds 2
}

if ($Mode -eq 'shim') {
    & $Unarr daemon install 2>&1 | Out-Null
    Start-Sleep -Seconds 3
    schtasks /run /tn unarr 2>&1 | Out-Null
} else {
    & $Unarr daemon uninstall 2>&1 | Out-Null
    # Not -Wait: it waits for descendants, and the detached daemon is one.
    $cli = Start-Process -FilePath $Unarr -ArgumentList 'daemon', 'start' -WorkingDirectory $WorkDir -PassThru -WindowStyle Hidden
    $null = $cli.WaitForExit(30000)
}

$up = WaitFor { $d = DaemonPids; $d.Count -gt 0 -and (Test-Path $State) -and ((Get-Content $State -Raw) -match "`"pid`":\s*$($d[0])") } 120
if (-not $up) { Say "ABORT: daemon never came up in mode $Mode"; exit 1 }
Say "daemon up: pid $(@(DaemonPids)[0])"
Say "state before logoff: $((Get-Content $State -Raw) -replace '\s+', ' ')"

# The probe runs at the next logon. Built as an array: no here-strings on 5.1.
$probe = @(
    '$s = "$env:LOCALAPPDATA\unarr\daemon.state.json"',
    "`$o = 'C:\unarr\logoff-probe-$Mode.txt'",
    '"probe ran at logon: $(Get-Date -Format o)" | Out-File $o -Encoding ascii',
    'if (Test-Path $s) { "STATE FILE PRESENT:" | Out-File $o -Append -Encoding ascii; Get-Content $s -Raw | Out-File $o -Append -Encoding ascii } else { "STATE FILE ABSENT" | Out-File $o -Append -Encoding ascii }',
    'if (Test-Path "$env:LOCALAPPDATA\unarr\daemon.stopped") { "stop-intent marker present" | Out-File $o -Append -Encoding ascii }',
    '"--- unarr.log tail ---" | Out-File $o -Append -Encoding ascii',
    'Get-Content "$env:LOCALAPPDATA\unarr\unarr.log" -Tail 25 -ErrorAction SilentlyContinue | Out-File $o -Append -Encoding ascii'
)
[System.IO.File]::WriteAllText("$WorkDir\logoff-probe.ps1", (($probe -join "`r`n") + "`r`n"), (New-Object System.Text.UTF8Encoding($true)))
$cmd = "powershell -NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File C:\unarr\logoff-probe.ps1"
Set-ItemProperty -Path 'HKCU:\Software\Microsoft\Windows\CurrentVersion\RunOnce' -Name 'unarrLogoffProbe' -Value $cmd
Say "probe armed in HKCU RunOnce; signing out in 5 s"
Start-Sleep -Seconds 5
shutdown /l
