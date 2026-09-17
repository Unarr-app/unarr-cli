# Host-event queries - REAL Windows only.
#
# A crash report's "host events" section is three wevtutil queries
# (cmd/unarr-desktop/hostevents.go). Their XPath is only parsed by the real
# wevtutil: a stray paren or a provider name Windows does not know comes back as
# an error, hostEventsSection prints "unavailable (exit status 1)", and the
# section that was supposed to say whether the machine went down under the
# daemon says nothing at all. Linux cannot catch that - hence this file.
#
# Run:  powershell -ExecutionPolicy Bypass -File \\host.lan\Data\smoke-hostevents.ps1
# Writes progress to \\host.lan\Data\hostevents-result.txt (watch from the host).
#
# ENCODING: deploy as UTF-8 WITH a BOM and CRLF, ASCII-only inside quotes - see
# the note at the top of smoke-crashreport.ps1 for what a BOM-less file does to
# PowerShell 5.1.

$ErrorActionPreference = 'Continue'
$Shared = '\\host.lan\Data'
$Out    = "$Shared\hostevents-result.txt"

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

Set-Content -Path $Out -Value "host-event queries - $(Get-Date)" -Encoding UTF8

# Same window the tray uses for a daemon that started a day ago, in the event
# log's own format.
$stamp = (Get-Date).ToUniversalTime().AddDays(-1).ToString("yyyy-MM-ddTHH:mm:ss.000Z")

# Kept in the SAME shape as crashEventsXPath / lowMemoryXPath / hostDownXPath in
# cmd/unarr-desktop/hostevents.go. Change one, change the other.
$queries = @(
    @{ Name = 'crash';      Log = 'Application'; XPath = "*[System[(EventID=1000 or EventID=1001 or EventID=1002) and TimeCreated[@SystemTime>='$stamp']]]" },
    @{ Name = 'low memory'; Log = 'System';      XPath = "*[System[Provider[@Name='Microsoft-Windows-Resource-Exhaustion-Detector'] and TimeCreated[@SystemTime>='$stamp']]]" },
    @{ Name = 'host down';  Log = 'System';      XPath = "*[System[(EventID=1074 or EventID=6006 or EventID=6008 or (EventID=41 and Provider[@Name='Microsoft-Windows-Kernel-Power'])) and TimeCreated[@SystemTime>='$stamp']]]" }
)

foreach ($q in $queries) {
    Say "query: $($q.Name)  (log $($q.Log))"
    # /c:20 /rd:true /f:text - exactly what queryEvents runs.
    $output = & wevtutil qe $q.Log "/q:$($q.XPath)" /f:text /rd:true /c:20 2>&1
    $code = $LASTEXITCODE
    # An empty result is a PASS: the query parsed, this machine simply has no
    # such event. Only a non-zero exit means the XPath itself was rejected.
    Check ($code -eq 0) "wevtutil accepted the XPath (exit $code)"
    if ($code -ne 0) {
        Say "    wevtutil said: $(($output | Select-Object -First 3) -join ' | ')"
    } else {
        $events = @($output | Where-Object { $_ -match '^Event\[' })
        Say "    $($events.Count) event(s) in the window"
    }
}

# The tray's own rendering of the same thing, end to end: `unarr-desktop` is not
# scriptable, so this just confirms the binary that ships carries the section.
$exe = 'C:\unarr\unarr-desktop.exe'
if (Test-Path $exe) {
    $strings = & findstr /C:"host events since" $exe
    Check ($LASTEXITCODE -eq 0) "the shipped tray binary carries the host-events section"
} else {
    Say "  [SKIP] $exe not deployed - run run.sh first for the binary check"
}

Say ""
Say "RESULT: $script:pass passed, $script:fail failed"
if ($script:fail -gt 0) { exit 1 }
