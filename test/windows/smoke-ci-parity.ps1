# smoke-ci-parity.ps1 - run the packages the GitHub windows-latest job runs, and
# report exactly which tests fail here.
#
# Why: `Test windows-latest` has been red on main, and a CI log is a poor place
# to iterate. These are the same package test binaries, cross-compiled, run on
# the same Windows this harness already boots.
#
# TWO THINGS THIS GETS RIGHT, both learned the hard way:
#
#   1. RUN FROM A LOCAL DIRECTORY. A process started from \\host.lan\Data
#      inherits a UNC working directory and every child it spawns then fails
#      (~72s of SMB timeout each). See the README's gotchas.
#   2. DEPLOY THE SOURCES. Several tests are AST guards or golden tests that
#      read their own package's .go files from the working directory. `go test`
#      runs with the cwd set to the package dir; a bare `go test -c` binary run
#      from anywhere else fails for that reason alone, which looks exactly like
#      a real Windows failure and is not one. src.zip carries the tree, and each
#      binary is run with its cwd set to its own package.
$ErrorActionPreference = 'Continue'
$share = '\\host.lan\Data'
$out = Join-Path $share 'ci-parity-result.txt'
$lines = @()
function Say($m) { $script:lines += $m; Write-Host $m }

# The engine tests bind sockets, and the first bind pops a Windows Defender
# firewall prompt that STEALS FOCUS - after which every keystroke the harness
# sends goes to the dialog instead of the shell, and the run is stuck. This is a
# throwaway test VM with a throwaway password and no real credentials, so the
# firewall is simply turned off for the duration rather than allow-listed binary
# by binary. Do this FIRST, before anything binds.
netsh advfirewall set allprofiles state off | Out-Null

$local = 'C:\unarrci'
Remove-Item -Recurse -Force $local -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path $local | Out-Null
Copy-Item (Join-Path $share '*_pkg_test.exe') $local -Force
Copy-Item (Join-Path $share 'unarr.exe') $local -Force -ErrorAction SilentlyContinue
Expand-Archive -Path (Join-Path $share 'src.zip') -DestinationPath $local -Force

Say "=== CI parity run on $env:COMPUTERNAME ==="
$map = @{
    'cmd_pkg_test.exe'     = 'internal\cmd'
    'logging_pkg_test.exe' = 'internal\logging'
    'doctor_pkg_test.exe'  = 'internal\doctor'
    'engine_pkg_test.exe'  = 'internal\engine'
}
$total = 0
foreach ($exe in $map.Keys | Sort-Object) {
    $path = Join-Path $local $exe
    if (-not (Test-Path $path)) { Say "SKIP: $exe not deployed"; continue }
    $pkgDir = Join-Path $local $map[$exe]
    if (-not (Test-Path $pkgDir)) { Say "SKIP: $exe - no sources at $pkgDir"; continue }

    Push-Location $pkgDir
    $o = & $path '-test.count=1' 2>&1 | Out-String
    Pop-Location

    $fails = [regex]::Matches($o, '(?m)^\s*--- FAIL: (\S+)') | ForEach-Object { $_.Groups[1].Value }
    if ($fails.Count -eq 0) {
        Say "PASS: $exe"
    } else {
        $total += $fails.Count
        Say "FAIL: $exe - $($fails.Count) test(s)"
        foreach ($f in $fails) { Say "    $f" }
        $ctx = ($o -split "`r?`n" | Select-String -Pattern '_test\.go:' -SimpleMatch | Select-Object -First 8)
        foreach ($c in $ctx) { Say "      $c" }
    }
}

Say ""
if ($total -eq 0) { Say "ALL PACKAGES PASSED" } else { Say "$total FAILING TEST(S)" }
[System.IO.File]::WriteAllLines($out, $lines, (New-Object System.Text.UTF8Encoding($false)))
