# Runs the engine package tests for the piece-completion quarantine fix on real
# Windows (bbolt uses LockFileEx here, os.Rename of a just-closed DB is the part a
# Linux run cannot prove). Copy the binary to a LOCAL dir first: a process
# started from the UNC share inherits a UNC cwd and every child fails (README).
$ErrorActionPreference = 'Continue'
$out = '\\host.lan\Data\engine-result.txt'
$local = 'C:\unarrtest'
$exe = Join-Path $local 'engine_test.exe'
$lines = @()   # ALWAYS an array: a scalar here turns '+=' into string concatenation or a throw
$utf8 = New-Object System.Text.UTF8Encoding($false)

# A stale result file must never read as a pass: remove it before doing anything.
if (Test-Path $out) { Remove-Item -Force $out }
# Same for a stale binary: a failed copy would otherwise run the previous build,
# and 'no tests to run' + EXIT=0 looks like a pass.
if (Test-Path $local) { Remove-Item -Recurse -Force $local }
New-Item -ItemType Directory -Force -Path $local | Out-Null
Copy-Item -Force \\host.lan\Data\engine_test.exe $exe
if (-not (Test-Path $exe)) {
    $lines += 'FAIL: engine_test.exe not copied from the share (rebuild it on the host, see README)'
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
