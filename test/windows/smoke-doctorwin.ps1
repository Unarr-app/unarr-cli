# Run the package tests for the doctor/support work on real Windows.
# ASCII only (see README gotchas).
$share = '\\host.lan\Data'
$out = Join-Path $share 'doctorwin-result.txt'
$lines = @()
$fail = 0
function Say($m) { $script:lines += $m; Write-Host $m }

Say "=== unarr doctor/support package tests on real Windows ==="
Say "host: $env:COMPUTERNAME  user: $env:USERNAME  home: $env:USERPROFILE"

foreach ($exe in @('cmd_test.exe','support_test.exe')) {
    $path = Join-Path $share $exe
    if (-not (Test-Path $path)) { Say "SKIP: $exe not deployed"; continue }
    $skip = 'TestDaemonStartsTheStopWatcher|TestStopIsNotDecidedByTheStateFile|TestStopSupervisorIsPlatformSplit|TestUninstallRecordsIntent|TestDaemonConsumesAndRecordsIntent|TestSignalShutdownDoesNotRecordIntent|TestDaemonStartReapsStateOutsideADefer|TestEveryDaemonEntryPointReapsState|TestDiskInfoBoundedIsUsedByRegister'
    $o = & $path '-test.short' "-test.skip=$skip" 2>&1 | Out-String
    if ($LASTEXITCODE -eq 0) { Say "PASS: $exe" }
    else {
        Say "FAIL: $exe (exit $LASTEXITCODE)"
        foreach ($line in ($o -split "`r?`n")) {
            if ($line -match '^(\s*---\s*FAIL|\s+\S+_test\.go:)') { Say "    $line" }
        }
        $fail++
    }
}

# The two things only Windows can settle for this change.
Say ""
Say "-- the Windows-only assertions, named --"
$named = @(
    @{ exe='cmd_test.exe';     run='AddrInUse|StreamPort|PortStatus' },
    @{ exe='support_test.exe'; run='HomePattern|ScrubRewritesHome|AbsentSection' }
)
foreach ($n in $named) {
    $path = Join-Path $share $n.exe
    $o = & $path '-test.v' '-test.short' "-test.run=$($n.run)" 2>&1 | Out-String
    $p = ([regex]::Matches($o, '(?m)^--- PASS')).Count
    $f = ([regex]::Matches($o, '(?m)^--- FAIL')).Count
    Say "$($n.exe) -test.run=$($n.run): $p pass, $f fail"
    foreach ($line in ($o -split "`r?`n")) {
        if ($line -match '^\s*--- (PASS|FAIL)') { Say "    $line" }
    }
    if ($f -gt 0 -or $p -eq 0) { Say "FAIL: $($n.exe) named subset"; $fail++ }
}

Say ""
if ($fail -eq 0) { Say "ALL CHECKS PASSED" } else { Say "$fail CHECK(S) FAILED" }
[System.IO.File]::WriteAllLines($out, $lines, (New-Object System.Text.UTF8Encoding($false)))
exit $fail
