# smoke-funnel-download.ps1 - does unarr actually fetch a working cloudflared on
# Windows?
#
# This is the one claim nothing else can check. The asset name, the pinned
# SHA-256 and the magic bytes are all tables that can be self-consistently wrong;
# only downloading on a real Windows shows whether the URL resolves, whether the
# bytes match what Cloudflare published, and whether the result runs.
#
# Before this, downloadCloudflared refused outright when GOOS != linux, so every
# Windows box with the funnel enabled logged "auto-download not supported" every
# five minutes forever.
$ErrorActionPreference = 'Continue'
$share = '\\host.lan\Data'
$out = Join-Path $share 'funnel-download-result.txt'
$local = 'C:\unarrci'
$lines = @()
function Say($m) { $script:lines += $m; Write-Host $m }

New-Item -ItemType Directory -Force -Path $local | Out-Null
Copy-Item (Join-Path $share 'funnel_pkg_test.exe') $local -Force
Push-Location $local

Say "=== funnel download on $env:COMPUTERNAME ==="
$env:UNARR_NET_TESTS = '1'
$o = & (Join-Path $local 'funnel_pkg_test.exe') '-test.v' '-test.count=1' '-test.run=TestDownloadCloudflaredForReal' 2>&1 | Out-String
Pop-Location

foreach ($line in ($o -split "`r?`n")) {
    if ($line -match 'install_network_test\.go:|^\s*(---|===) (PASS|FAIL|SKIP)|^(ok|FAIL|PASS)') { Say $line.Trim() }
}
if ($o -match '(?m)^--- PASS') { Say ""; Say "RESULT: PASS - Windows downloads, verifies and runs cloudflared" }
elseif ($o -match '(?m)^--- SKIP') { Say ""; Say "RESULT: SKIP" }
else { Say ""; Say "RESULT: FAIL" }

[System.IO.File]::WriteAllLines($out, $lines, (New-Object System.Text.UTF8Encoding($false)))
