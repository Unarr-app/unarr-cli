# Torrent engine start when Windows reserves its listen port - REAL Windows, ELEVATED.
#
# Windows reserves blocks of ports for Hyper-V, WinNAT, WSL and Docker Desktop
# ("excluded port ranges"). A bind inside one fails with WSAEACCES, "An attempt
# was made to access a socket in a way forbidden by its access permissions" - not
# "address in use". GitHub's windows runners hit it on random ports; a user whose
# reservation covers 42069 would hit it on every start, and the engine's port walk
# only understood "in use", so the torrent engine never came up at all.
#
# This reserves a UDP block starting at 42069 (anacrolix binds TCP and then uTP on
# the SAME number, so UDP alone is enough), proves a plain UDP bind there is
# refused, and runs the engine's harness test against it.
#
# Run (elevated):  powershell -ExecutionPolicy Bypass -File \\host.lan\Data\smoke-portrange.ps1
# Needs engine_test.exe on the share:
#   GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -c -o test/windows/shared/engine_test.exe ./internal/engine
#
# ENCODING: deploy as UTF-8 WITH a BOM and CRLF; keep this file ASCII.

$ErrorActionPreference = 'Continue'
$Shared  = '\\host.lan\Data'
$Out     = "$Shared\portrange-result.txt"
$WorkDir = 'C:\unarr'
$Port    = 42069
$Block   = 50

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

Remove-Item $Out -ErrorAction SilentlyContinue
Say "=== excluded port range vs the torrent engine on $(([System.Environment]::OSVersion).VersionString) ==="

$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
    [Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $admin) { Say "ABORT: run from an elevated PowerShell (netsh excludedportrange needs admin)"; Say "DONE"; exit 2 }

New-Item -ItemType Directory -Force -Path $WorkDir | Out-Null
Copy-Item "$Shared\engine_test.exe" $WorkDir -Force

Say "[1] reserve udp $Port-$($Port + $Block - 1) (active store only, gone at reboot)"
netsh int ipv4 delete excludedportrange protocol=udp startport=$Port numberofports=$Block store=active 2>&1 | Out-Null
$add = netsh int ipv4 add excludedportrange protocol=udp startport=$Port numberofports=$Block store=active 2>&1 | Out-String
Say "  netsh| $($add.Trim())"
$shown = netsh int ipv4 show excludedportrange protocol=udp | Out-String
Check ($shown -match "\b$Port\b") "the reservation is listed by 'show excludedportrange'"

Say "[2] baseline: a plain UDP bind on $Port is refused by policy, not by a holder"
$refused = $false
$code = 0
try {
    $u = New-Object System.Net.Sockets.UdpClient($Port)
    $u.Close()
} catch {
    $inner = $_.Exception.InnerException
    if ($inner -and $inner.ErrorCode) { $code = $inner.ErrorCode }
    $refused = $true
}
Check ($refused -and $code -eq 10013) "UDP bind on $Port fails with WSAEACCES 10013 (got refused=$refused code=$code)"

Say "[3] the torrent engine starts anyway"
$env:UNARR_HARNESS_FORBIDDEN_PORT = "$Port"
Push-Location $WorkDir
$res = & "$WorkDir\engine_test.exe" '-test.v' '-test.run' 'TestHarnessForbiddenListenPort' 2>&1 | Out-String
$exit = $LASTEXITCODE
Pop-Location
Remove-Item Env:UNARR_HARNESS_FORBIDDEN_PORT -ErrorAction SilentlyContinue
$res -split "`n" | Where-Object { $_ -match '(--- |^ok|^FAIL|^PASS|\[torrent\] (port|listening))' } | ForEach-Object { Say "  go| $($_.TrimEnd())" }
Check ($exit -eq 0 -and $res -match '--- PASS: TestHarnessForbiddenListenPort') "engine harness test passed (exit $exit)"
Check ($res -match 'was unusable') "the engine logged that it walked off the reserved port"

Say "[teardown] release the reservation"
netsh int ipv4 delete excludedportrange protocol=udp startport=$Port numberofports=$Block store=active 2>&1 | Out-Null

Say ""
Say "RESULT passed=$($script:pass) failed=$($script:fail)"
Say "DONE"
if ($script:fail -gt 0) { exit 1 } else { exit 0 }
