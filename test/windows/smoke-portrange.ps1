# Torrent engine start when Windows refuses its listen port - REAL Windows, ELEVATED.
#
# anacrolix binds TCP and then uTP (UDP) on the SAME port number. On Windows the
# UDP bind can fail with WSAEACCES, "An attempt was made to access a socket in a
# way forbidden by its access permissions" - not "address in use". GitHub's
# windows runners hit it on random ports; the engine's port walk only understood
# "in use", so a refused 42069 stopped the torrent engine from starting at all.
#
# Ways to make Windows refuse the port, each measured here:
#   [1]/[1b]/[1c] an excluded port range added with `netsh int ipv4 add
#       excludedportrange` (what Hyper-V/WinNAT/Docker reserve), udp active,
#       udp persistent and tcp persistent. Informational: on Windows 11 26200
#       (2026-09-14) NONE of them refused an explicit bind (code 0), so a
#       netsh reservation cannot stand in for WSAEACCES on this VM. If one ever
#       does refuse with 10013, the engine is run against it and must jump a
#       whole block (+100).
#   [2]/[3] another socket holding the udp port with SO_EXCLUSIVEADDRUSE. A
#       plain bind then gets 10048 (WSAEADDRINUSE), measured; the engine must
#       walk off to the next port. The WSAEACCES branch itself is covered by
#       internal/engine/listenport_windows_test.go with the real errno.
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
function NewUdp { New-Object System.Net.Sockets.Socket([Net.Sockets.AddressFamily]::InterNetwork, [Net.Sockets.SocketType]::Dgram, [Net.Sockets.ProtocolType]::Udp) }
# Bind a plain UDP socket to 0.0.0.0:$p; returns 0 on success, else the Winsock code.
function ProbeUdpBind([int]$p) {
    $s = NewUdp
    try {
        $s.Bind((New-Object Net.IPEndPoint([Net.IPAddress]::Any, $p)))
        return 0
    } catch {
        $ex = $_.Exception
        while ($ex.InnerException) { $ex = $ex.InnerException }
        if ($ex.ErrorCode) { return [int]$ex.ErrorCode }
        return -1
    } finally { $s.Close() }
}
function RunEngine($tag) {
    $env:UNARR_HARNESS_FORBIDDEN_PORT = "$Port"
    Push-Location $WorkDir
    $res = & "$WorkDir\engine_test.exe" '-test.v' '-test.run' 'TestHarnessForbiddenListenPort' 2>&1 | Out-String
    $code = $LASTEXITCODE
    Pop-Location
    Remove-Item Env:UNARR_HARNESS_FORBIDDEN_PORT -ErrorAction SilentlyContinue
    $res -split "`n" | Where-Object { $_ -match '(--- |^ok|^FAIL|^PASS|\[torrent\] (port|listening))' } | ForEach-Object { Say "  go($tag)| $($_.TrimEnd())" }
    return @{ Out = $res; Code = $code }
}

Remove-Item $Out -ErrorAction SilentlyContinue
Say "=== refused listen port vs the torrent engine on $(([System.Environment]::OSVersion).VersionString) ==="

$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
    [Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $admin) { Say "ABORT: run from an elevated PowerShell (netsh excludedportrange needs admin)"; Say "DONE"; exit 2 }

New-Item -ItemType Directory -Force -Path $WorkDir | Out-Null
Copy-Item "$Shared\engine_test.exe" $WorkDir -Force

# -- 1. netsh excluded port range (measured, informational) -------------------
Say "[1] excluded port range udp $Port-$($Port + $Block - 1), active store"
netsh int ipv4 delete excludedportrange protocol=udp startport=$Port numberofports=$Block store=active 2>&1 | Out-Null
$add = netsh int ipv4 add excludedportrange protocol=udp startport=$Port numberofports=$Block store=active 2>&1 | Out-String
Say "  netsh| $($add.Trim())"
$shown = netsh int ipv4 show excludedportrange protocol=udp | Out-String
Say "  [info] reservation listed: $($shown -match "\b$Port\b")"
$rc = ProbeUdpBind $Port
Say "  [info] explicit UDP bind inside the reservation -> code $rc (0 = allowed)"
netsh int ipv4 delete excludedportrange protocol=udp startport=$Port numberofports=$Block store=active 2>&1 | Out-Null

# The persistent store is what winnat/Hyper-V write; measured separately because
# the active store turned out NOT to refuse an explicit bind (code 0).
Say "[1b] excluded port range udp $Port-$($Port + $Block - 1), persistent store"
netsh int ipv4 delete excludedportrange protocol=udp startport=$Port numberofports=$Block 2>&1 | Out-Null
$add = netsh int ipv4 add excludedportrange protocol=udp startport=$Port numberofports=$Block 2>&1 | Out-String
Say "  netsh| $($add.Trim())"
$rcP = ProbeUdpBind $Port
Say "  [info] explicit UDP bind inside the persistent reservation -> code $rcP (0 = allowed)"
if ($rcP -eq 10013) {
    $r = RunEngine 'excluded'
    Check ($r.Code -eq 0 -and $r.Out -match '--- PASS: TestHarnessForbiddenListenPort') "engine starts inside an excluded range (exit $($r.Code))"
    Check ($r.Out -match "listening on port $($Port + 100)\b") "WSAEACCES jumped a whole block: listening on $($Port + 100)"
}
netsh int ipv4 delete excludedportrange protocol=udp startport=$Port numberofports=$Block 2>&1 | Out-Null
Say "  [info] reservation still listed after teardown: $((netsh int ipv4 show excludedportrange protocol=udp | Out-String) -match "\b$Port\b")"

# Hyper-V/Docker "forbidden by its access permissions" reports are about TCP,
# and anacrolix binds TCP first ("first listen").
Say "[1c] excluded port range tcp $Port-$($Port + $Block - 1), persistent store"
netsh int ipv4 delete excludedportrange protocol=tcp startport=$Port numberofports=$Block 2>&1 | Out-Null
$add = netsh int ipv4 add excludedportrange protocol=tcp startport=$Port numberofports=$Block 2>&1 | Out-String
Say "  netsh| $($add.Trim())"
$rcT = 0
$l = New-Object Net.Sockets.TcpListener([Net.IPAddress]::Any, $Port)
try { $l.Start() } catch {
    $ex = $_.Exception; while ($ex.InnerException) { $ex = $ex.InnerException }
    $rcT = if ($ex.ErrorCode) { [int]$ex.ErrorCode } else { -1 }
} finally { $l.Stop() }
Say "  [info] explicit TCP listen inside the reservation -> code $rcT (0 = allowed)"
if ($rcT -eq 10013) {
    $r = RunEngine 'excluded-tcp'
    Check ($r.Code -eq 0 -and $r.Out -match '--- PASS: TestHarnessForbiddenListenPort') "engine starts inside an excluded TCP range (exit $($r.Code))"
    Check ($r.Out -match "listening on port $($Port + 100)\b") "WSAEACCES jumped a whole block: listening on $($Port + 100)"
}
netsh int ipv4 delete excludedportrange protocol=tcp startport=$Port numberofports=$Block 2>&1 | Out-Null
Say "  [info] tcp reservation still listed after teardown: $((netsh int ipv4 show excludedportrange protocol=tcp | Out-String) -match "\b$Port\b")"

# -- 2. an exclusive holder ----------------------------------------------------
Say "[2] another socket holds udp $Port with SO_EXCLUSIVEADDRUSE"
$holder = NewUdp
$holder.ExclusiveAddressUse = $true
$held = $true
try { $holder.Bind((New-Object Net.IPEndPoint([Net.IPAddress]::Any, $Port))) } catch { $held = $false; Say "  holder bind failed: $($_.Exception.Message)" }
Check $held "exclusive holder bound udp $Port"
$rc = ProbeUdpBind $Port
Say "  [info] a plain UDP bind against the exclusive holder -> code $rc (10013 WSAEACCES, 10048 WSAEADDRINUSE)"
Check ($rc -ne 0) "the port is refused to a second socket (code $rc)"

Say "[3] the torrent engine starts with its configured port refused"
$r = RunEngine 'held'
Check ($r.Code -eq 0 -and $r.Out -match '--- PASS: TestHarnessForbiddenListenPort') "engine harness test passed (exit $($r.Code))"
Check ($r.Out -match 'was unusable') "the engine logged that it walked off the refused port"
switch ($rc) {
    10013 { Check ($r.Out -match "listening on port $($Port + 100)\b") "WSAEACCES jumped a whole block: listening on $($Port + 100)" }
    10048 { Check ($r.Out -match "listening on port $($Port + 1)\b") "WSAEADDRINUSE stepped to the neighbour: listening on $($Port + 1)" }
    default { Say "  [info] refusal code $rc is neither 10013 nor 10048 - step size not asserted" }
}
$holder.Close()

Say ""
Say "RESULT passed=$($script:pass) failed=$($script:fail)"
Say "DONE"
if ($script:fail -gt 0) { exit 1 } else { exit 0 }
