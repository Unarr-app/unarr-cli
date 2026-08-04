# smoke-ci-detail.ps1 - full verbose output for the tests still failing on
# Windows, so the reason is readable instead of guessed at.
$ErrorActionPreference = 'Continue'
$share = '\\host.lan\Data'
$out = Join-Path $share 'ci-detail-result.txt'
$local = 'C:\unarrci'
$lines = @()

$targets = @(
    @{ exe = 'cmd_pkg_test.exe';     dir = 'internal\cmd';     run = 'TestWriteServiceFileIsReadableByTheSupervisor' },
    @{ exe = 'engine_pkg_test.exe';  dir = 'internal\engine';  run = 'TestMoveFileReportsAnUndeletableSource' },
    @{ exe = 'logging_pkg_test.exe'; dir = 'internal\logging'; run = 'TestFollowSurvivesRotation|TestWriterBacksOffAndReportsOnceWhenTheRenameFails' }
)
foreach ($t in $targets) {
    $lines += ""
    $lines += "########## $($t.exe) -test.run=$($t.run)"
    Push-Location (Join-Path $local $t.dir)
    $o = & (Join-Path $local $t.exe) '-test.v' '-test.count=1' "-test.run=$($t.run)" 2>&1 | Out-String
    Pop-Location
    $lines += ($o -split "`r?`n" | Select-Object -First 60)
}
[System.IO.File]::WriteAllLines($out, $lines, (New-Object System.Text.UTF8Encoding($false)))
Write-Host "done"
