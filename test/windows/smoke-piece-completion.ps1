# Runs the engine package tests for the piece-completion quarantine fix on real
# Windows (bbolt uses LockFileEx here, os.Rename of a just-closed DB is the part a
# Linux run cannot prove). Copy the binary to a LOCAL dir first: a process
# started from the UNC share inherits a UNC cwd and every child fails (README).
$ErrorActionPreference = 'Continue'
# $env:UNARR_TEST_BIN picks another package test binary from the share (e.g.
# cmd_test.exe); the result file follows its name.
$bin = $env:UNARR_TEST_BIN
if (-not $bin) { $bin = 'engine_test.exe' }
$out = '\\host.lan\Data\' + ($bin -replace '_test\.exe$', '') + '-result.txt'
$local = 'C:\unarrtest'
$exe = Join-Path $local $bin
$lines = @()   # ALWAYS an array: a scalar here turns '+=' into string concatenation or a throw
$utf8 = New-Object System.Text.UTF8Encoding($false)

# A stale result file must never read as a pass: remove it before doing anything.
if (Test-Path $out) { Remove-Item -Force $out }
# Same for a stale binary: a failed copy would otherwise run the previous build,
# and 'no tests to run' + EXIT=0 looks like a pass.
if (Test-Path $local) { Remove-Item -Recurse -Force $local }
New-Item -ItemType Directory -Force -Path $local | Out-Null
Copy-Item -Force (Join-Path '\\host.lan\Data' $bin) $exe
if (-not (Test-Path $exe)) {
    $lines += "FAIL: $bin not copied from the share (rebuild it on the host, see README)"
    $lines += 'EXIT=99'
    [System.IO.File]::WriteAllLines($out, $lines, $utf8)
    exit 99
}
Set-Location $local
$sw = [System.Diagnostics.Stopwatch]::StartNew()
# Each flag quoted: PS 5.1 splits a bare -test.v into '-test' + '.v'.
# $env:UNARR_ENGINE_RUN overrides the test filter ('.' = the whole package).
$run = $env:UNARR_ENGINE_RUN
if (-not $run) { $run = 'PieceCompletion|InjectedDamage|RepairsDamaged|FreelistOnly' }
$lines += @(& $exe '-test.v' '-test.run' $run 2>&1 | ForEach-Object { "$_" })
$code = $LASTEXITCODE
if ($code -eq $null) { $code = 98; $lines += 'FAIL: the test binary did not launch (wrong arch? SmartScreen?)' }
$lines += "EXIT=$code ELAPSED=$($sw.Elapsed.TotalSeconds)s"
$lines += "OS=" + [System.Environment]::OSVersion.VersionString
[System.IO.File]::WriteAllLines($out, $lines, $utf8)
