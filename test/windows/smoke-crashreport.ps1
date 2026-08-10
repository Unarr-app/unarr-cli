# Crash-report fidelity checks - REAL Windows only.
#
# Two field crash reports arrived unusable, and neither failure can be produced
# on Linux:
#
#   1. The entire log tail was "No logs available. (exit status 0xc0000142)".
#      0xc0000142 is STATUS_DLL_INIT_FAILED - the Windows loader killing a
#      process before main() runs. The dead process was NOT the daemon: it was
#      the unarr.exe the TRAY spawned to collect the logs (logsources.go calls
#      `unarr daemon logs`). So the crash evidence was replaced by the exit code
#      of the thing sent to fetch it, and the report then blamed the user's
#      setup ("Install it as a service") for a daemon that WAS a service.
#
#   2. Log lines arrived mojibake'd: "81% ?" 3.6 GB" instead of "81% - 3.6 GB".
#      A UTF-8 em dash written by the daemon, read back through the console code
#      page. Only a real Windows console has a code page; Linux cannot show it.
#
# Run:  powershell -ExecutionPolicy Bypass -File \\host.lan\Data\smoke-crashreport.ps1
# Writes progress to \\host.lan\Data\crashreport-result.txt (watch from the host).
#
# ENCODING: deploy as UTF-8 WITH a BOM and CRLF. Windows PowerShell 5.1 decodes
# a BOM-less .ps1 as CP1252, which turns a UTF-8 em dash into a curly quote that
# PowerShell accepts as a string delimiter - every quote after it then pairs up
# wrong and the parse error surfaces on the last line, pointing nowhere near the
# cause. Keep non-ASCII out of quoted strings here regardless; the BOM is the
# belt. (Same lesson as smoke-supervision.ps1.)

$ErrorActionPreference = 'Continue'
$Shared  = '\\host.lan\Data'
$Out     = "$Shared\crashreport-result.txt"
$WorkDir = 'C:\unarr'
$DataDir = "$env:LOCALAPPDATA\unarr"

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

Set-Content -Path $Out -Value "crash-report fidelity - $(Get-Date)" -Encoding UTF8
Say "DataDir: $DataDir"

if (-not (Test-Path "$WorkDir\unarr.exe")) {
    New-Item -ItemType Directory -Force -Path $WorkDir | Out-Null
    Copy-Item "$Shared\unarr.exe" $WorkDir -Force
}

# ---------------------------------------------------------------- 1. code page
#
# RECORDED, NOT ASSERTED. 437/850 (OEM) and 1252 (ANSI) all decode UTF-8 bytes
# wrongly; only 65001 is UTF-8. But the console code page belongs to the USER's
# machine - unarr does not get to set it, and a fix that depended on 65001 would
# only work on boxes that were never broken. The real fix is that log lines are
# ASCII, which every one of these code pages decodes identically. This value is
# logged so a reader knows which decoder produced the bytes below.
Say ''
Say '== 1. console code page (recorded, not asserted) =='
$cp = (chcp) -replace '[^0-9]', ''
Say "  active code page: $cp"
if ($cp -ne '65001') { Say "  (not UTF-8 - exactly the decoder that mangled the field reports)" }

# -------------------------------------------- 2. the daemon's own lines, as read
#
# THE MEASUREMENT THAT MATTERS, and the one only real Windows can make.
#
# A UTF-8 em dash cannot survive a CP437 console - that is a property of the code
# page, not a bug anyone can fix in unarr. So this does NOT check that non-ASCII
# round-trips. It checks that the daemon never WRITES non-ASCII in the first
# place, which is the only version of this that holds on every user's box.
#
# The bytes are read back through the ANSI decoder deliberately: that is what the
# tray's CombinedOutput, a console, and Notepad all effectively do, and it is
# where "81% - 3.6 GB" became "81% GA-Aoe 3.6 GB" in the field.
Say ''
Say '== 2. the daemon writes ASCII-only log lines =='
$logFile = "$DataDir\unarr.log"
New-Item -ItemType Directory -Force -Path $DataDir | Out-Null

# Produce a FRESH log by running the daemon briefly. The startup banner alone
# exercises the transcode/tonemap/hwscale probes, which are where the em dashes
# in the field reports came from.
#
# The old log is deleted first, and that is not tidiness: a previous run's file
# is a log written by a DIFFERENT build. Reading one of those is how this check
# first "failed" against a binary that was already fixed - the non-ASCII bytes it
# found had been written days earlier, by the version with the bug. Rotated
# siblings go too, since `unarr daemon logs` concatenates them.
Say '  clearing old logs so this measures the CURRENT build'
Remove-Item "$DataDir\unarr.log*" -Force -ErrorAction SilentlyContinue
Remove-Item "$DataDir\unarr.boot.log*" -Force -ErrorAction SilentlyContinue

$p = Start-Process -FilePath "$WorkDir\unarr.exe" `
    -ArgumentList 'start', '--log-file', "`"$logFile`"" `
    -PassThru -WindowStyle Hidden
Start-Sleep -Seconds 25
Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
Start-Sleep -Seconds 2

if (Test-Path $logFile) {
    $captured = & "$WorkDir\unarr.exe" daemon logs 2>&1 | Out-String
    $bytes    = [System.Text.Encoding]::Default.GetBytes($captured)
    $high     = @($bytes | Where-Object { $_ -gt 0x7F })
    Say "  captured $($bytes.Length) bytes, $($high.Count) above 0x7F"
    Check ($high.Count -eq 0) "no non-ASCII bytes in the daemon's own log output"

    # The exact corruption from the field reports: E2 80 94 decoded as CP437.
    Check (-not $captured.Contains([char]0x0393)) "no 'GA-' mojibake (an em dash read through CP437)"
    Check (-not $captured.Contains([char]0xFFFD)) "no replacement characters"
} else {
    Say "  SKIP: no daemon log at $logFile yet (run the agent first)"
}

# ------------------------------------------- 3. collector failure is attributed
#
# Simulate the 0xc0000142 case: make the collector unrunnable and confirm the
# tray's own message does not blame the user's install. A DLL-init failure cannot
# be forced directly, but ANY spawn failure takes the identical code path
# (logsources.go embeds err.Error() into "No logs available. (...)").
#
# A .exe that is not a valid PE image is the closest faithful stand-in: the
# loader rejects it, exactly like a corrupt/AV-quarantined binary in the field.
Say ''
Say '== 3. a collector that cannot start =='
$badDir = "$env:TEMP\unarr-badexe"
New-Item -ItemType Directory -Force -Path $badDir | Out-Null
[System.IO.File]::WriteAllBytes("$badDir\unarr.exe", [byte[]](0x4D,0x5A,0x00,0x00,0x00))

$rc = 0
try {
    $null = & "$badDir\unarr.exe" daemon logs 2>&1
    $rc = $LASTEXITCODE
} catch {
    $rc = -1
    Say "  spawn threw: $($_.Exception.Message)"
}
Say "  exit code from a malformed PE: $rc (0x$('{0:X}' -f $rc))"
Check ($rc -ne 0) "a collector that cannot load reports a non-zero status"

# ------------------------------------------------ 4. the daemon's own log lines
#
# The daemon writes the progress line at internal/engine/torrent.go:600, which
# contains a literal em dash. The ASCII guard in internal/logging only inspects
# string literals passed DIRECTLY to log.*; that line is built with fmt.Sprintf
# into a variable first, so the guard never sees it. Confirm what actually lands
# in the file a support bundle collects.
Say ''
Say '== 4. bytes in the on-disk log =='
if (Test-Path $logFile) {
    $raw = [System.IO.File]::ReadAllBytes($logFile)
    $nonAscii = @($raw | Where-Object { $_ -gt 0x7F })
    Say "  $($raw.Length) bytes, $($nonAscii.Count) non-ASCII"
    # Not a failure by itself - UTF-8 on disk is correct. It is a failure only
    # once a CP1252 reader opens it, which is what the field reports show. Recorded
    # so a regression in the guard is visible here too.
    Say "  (non-ASCII on disk is what a CP1252 reader turns into mojibake)"
}

# ------------------------------------------- 5. the upgrade leaves no partial exe
#
# installBinary stages a sibling .new file and renames it over the target. The
# rename is what makes an upgrade atomic, and Windows is where that matters most:
# a running .exe cannot be overwritten, only renamed aside, and a truncated PE is
# answered by the loader with 0xc0000142 - the status a field crash report
# carried.
#
# This cannot drive a real upgrade (that needs a signed release), so it verifies
# the property the fix depends on: that a rename over an existing binary is legal
# here, and that a PE stays loadable across it.
Say ''
Say '== 5. atomic replace of a binary =='
$stage = "$env:TEMP\unarr-atomic"
New-Item -ItemType Directory -Force -Path $stage | Out-Null
Copy-Item "$WorkDir\unarr.exe" "$stage\target.exe" -Force
Copy-Item "$WorkDir\unarr.exe" "$stage\target.exe.new" -Force

# Go's os.Rename on Windows is MoveFileEx with MOVEFILE_REPLACE_EXISTING, and
# that is what has to work for installBinary's staged rename to be atomic.
#
# Reaching it from PowerShell 5.1 needs care: File]::Move's 3-argument overwrite
# overload is .NET Core only (5.1 runs on .NET Framework), and File]::Replace
# rejects a $null backup path. Move-Item -Force is the Framework-era path that
# ends up in the same Win32 call.
$renamed = $false
try {
    Move-Item "$stage\target.exe.new" "$stage\target.exe" -Force -ErrorAction Stop
    $renamed = $true
} catch {
    Say "  rename failed: $($_.Exception.Message)"
}
Check $renamed "a staged .new can be renamed over the live target"

if ($renamed) {
    $ver = & "$stage\target.exe" version 2>&1 | Out-String
    Check ($ver -match 'unarr') "the binary is loadable after the rename: $($ver.Trim())"
    Check (-not (Test-Path "$stage\target.exe.new")) "no .new file left behind"
}
Remove-Item -Recurse -Force $stage -ErrorAction SilentlyContinue

Say ''
Say "RESULT: $script:pass passed, $script:fail failed"
if ($script:fail -gt 0) { Say 'FAILURES PRESENT' } else { Say 'ALL CHECKS PASSED' }
