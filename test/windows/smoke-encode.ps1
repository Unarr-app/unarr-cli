# Real encodes through StartHLSSession, from BOTH kinds of working directory a
# daemon can be started in. Build the test binary first (it needs the smoke tag):
#   GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -c -tags=smoke -o test/windows/shared/engine_smoke.exe ./internal/engine
# ffmpeg.exe + ffprobe.exe must be on the share too. Run NOT elevated: an elevated
# shell can write into System32 and would pass with the bug still in place.
#   powershell -ExecutionPolicy Bypass -File \\host.lan\Data\smoke-encode.ps1
$share = '\\host.lan\Data'
$dir = 'C:\unarrtest'
New-Item -ItemType Directory -Force -Path $dir | Out-Null
foreach ($f in 'ffmpeg.exe', 'ffprobe.exe', 'engine_smoke.exe') {
    Copy-Item "$share\$f" "$dir\$f" -Force
}
$env:PATH = "$dir;$env:PATH"
Remove-Item "$dir\init.mp4" -Force -ErrorAction SilentlyContinue

$elevated = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
$lines = @("elevated=$elevated")
$failed = 0
if ($elevated) {
    $lines += 'FAIL: run this NOT elevated, System32 must refuse the write'
    $failed++
}

# System32 is where the scheduled task starts the daemon: no WorkingDirectory.
foreach ($cwd in 'C:\Windows\System32', $dir) {
    Set-Location $cwd
    $out = & "$dir\engine_smoke.exe" '-test.v' '-test.run' 'TestEncodeSessionProducesFirstSegment|TestRejectedSourceIsRecognised|TestEncoderFailureCarriesStderr' '-test.timeout' '20m' 2>&1 | Out-String
    $code = $LASTEXITCODE
    $pass = ([regex]::Matches($out, '--- PASS: \S+/')).Count
    $fail = ([regex]::Matches($out, '--- FAIL: \S+/')).Count
    $lines += "cwd=$cwd exit=$code subtests_pass=$pass subtests_fail=$fail"
    if ($code -ne 0 -or $pass -eq 0) {
        $failed++
        $lines += ($out -split "`n" | Select-String -Pattern 'Permission denied|encoder gave up|ServeInit|working directory' | Select-Object -First 8 | ForEach-Object { $_.ToString().Trim() })
    }
}
Set-Location $dir
if (Test-Path "$dir\init.mp4") {
    $lines += 'FAIL: stray init.mp4 in the writable working directory'
    $failed++
}
$lines += "RESULT failed=$failed"
$lines | Out-File -Encoding ascii "$share\encode-result.txt"
$lines | ForEach-Object { Write-Host $_ }
exit $failed
