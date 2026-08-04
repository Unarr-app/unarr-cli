# smoke-engine-repeat.ps1 - is internal/engine failing on Windows, or flaking?
# Three consecutive runs; a set of failures that changes between runs is a
# timing problem, one that repeats is a real defect.
$ErrorActionPreference = 'Continue'
$share = '\\host.lan\Data'
$out = Join-Path $share 'engine-repeat-result.txt'
$local = 'C:\unarrci'
$lines = @()

Push-Location (Join-Path $local 'internal\engine')
for ($i = 1; $i -le 3; $i++) {
    $o = & (Join-Path $local 'engine_pkg_test.exe') '-test.count=1' '-test.v' 2>&1 | Out-String
    $fails = [regex]::Matches($o, '(?m)^\s*--- FAIL: (\S+)') | ForEach-Object { $_.Groups[1].Value }
    $lines += "run ${i}: $($fails.Count) failure(s): $($fails -join ', ')"
    foreach ($f in $fails) {
        # the assertion line for this test, which says why
        $idx = ($o -split "`r?`n") | Select-String -Pattern ([regex]::Escape($f)) -Context 0, 0
        $ctx = ($o -split "`r?`n" | Select-String -Pattern '_test\.go:' -SimpleMatch | Select-Object -First 4)
        foreach ($c in $ctx) { $lines += "    $c" }
        break
    }
}
Pop-Location
[System.IO.File]::WriteAllLines($out, $lines, (New-Object System.Text.UTF8Encoding($false)))
Write-Host done
