# Exercises the REAL `unarr daemon install` path on Windows, not a hand-rolled
# copy of the netsh call. Measuring a copy of the logic only measures the copy.
#
# What must hold: install adds the program-scoped inbound rule pointing at the
# INSTALLED binary, and uninstall removes it (plus any legacy port-scoped rules
# an older build left behind).

$ErrorActionPreference = 'Continue'
$out  = "\\host.lan\Data\fw-install-result.txt"
$rule = "unarr (BitTorrent peers)"
$binDir = "C:\unarr-test"
$bin    = "$binDir\unarr.exe"
$res  = New-Object System.Collections.ArrayList
function Note($s) { [void]$res.Add($s); Write-Host $s }
function Chk($n,$ok,$d) { if ($ok) { Note ("PASS  {0}  {1}" -f $n,$d) } else { Note ("FAIL  {0}  {1}" -f $n,$d) } }

Note "=== unarr daemon install firewall smoke  $(Get-Date -Format o) ==="
New-Item -ItemType Directory -Force -Path $binDir | Out-Null
Copy-Item "\\host.lan\Data\unarr.exe" $bin -Force
Unblock-File $bin -ErrorAction SilentlyContinue

# Clean slate: no rule, no task.
netsh advfirewall firewall delete rule name="$rule" 2>&1 | Out-Null
& $bin daemon uninstall 2>&1 | Out-Null
Start-Sleep 2

# --- install ---------------------------------------------------------------
try {
  $log = & $bin daemon install 2>&1 | Out-String
  Note ("--- install output ---`n" + $log.Trim())
  Chk "install-exit" ($LASTEXITCODE -eq 0) "exit=$LASTEXITCODE"
  # The install must SAY what it did about the firewall, either way.
  Chk "install-mentions-firewall" ($log -match '(?i)firewall') "install output names the firewall"
} catch { Chk "install-exit" $false $_.Exception.Message }

# --- the rule the CODE created --------------------------------------------
try {
  $show = netsh advfirewall firewall show rule name="$rule" verbose 2>&1 | Out-String
  Chk "rule-created-by-install" ($show -notmatch 'No rules match') "rule exists after install"
  Chk "rule-points-at-installed-binary" ($show -match [regex]::Escape($bin)) `
      (($show | Select-String '(?i)Program:.*') -join '').Trim()
  Chk "rule-all-profiles" ($show -match '(?i)Profiles:\s*(Domain,Private,Public|Any)') `
      (($show | Select-String '(?i)Profiles:.*') -join '').Trim()
} catch { Chk "rule-created-by-install" $false $_.Exception.Message }

# --- Defender reaction to the REAL install --------------------------------
try {
  $threats = @(Get-MpThreatDetection -ErrorAction SilentlyContinue |
               Where-Object { $_.InitialDetectionTime -gt (Get-Date).AddMinutes(-10) })
  Chk "defender-quiet-on-install" ($threats.Count -eq 0) ("detections: " + $threats.Count)
  $ev = @(Get-WinEvent -LogName "Microsoft-Windows-Windows Defender/Operational" `
          -MaxEvents 40 -ErrorAction SilentlyContinue |
          Where-Object { $_.Id -in 1006,1015,1116,1117 -and $_.TimeCreated -gt (Get-Date).AddMinutes(-10) })
  Chk "defender-no-events-on-install" ($ev.Count -eq 0) ("events: " + $ev.Count)
} catch { Note ("INFO  defender check unavailable: " + $_.Exception.Message) }

# --- uninstall removes it --------------------------------------------------
try {
  $u = & $bin daemon uninstall 2>&1 | Out-String
  Start-Sleep 2
  $show2 = netsh advfirewall firewall show rule name="$rule" 2>&1 | Out-String
  Chk "uninstall-removes-rule" ($show2 -match 'No rules match') "rule gone after uninstall"
} catch { Chk "uninstall-removes-rule" $false $_.Exception.Message }

# --- legacy port-scoped rules are cleaned too ------------------------------
try {
  netsh advfirewall firewall add rule name="unarr (peer TCP)" dir=in action=allow `
    protocol=TCP localport=42069 2>&1 | Out-Null
  & $bin daemon uninstall 2>&1 | Out-Null
  Start-Sleep 2
  $legacy = netsh advfirewall firewall show rule name="unarr (peer TCP)" 2>&1 | Out-String
  Chk "uninstall-clears-legacy-rule" ($legacy -match 'No rules match') "old port rule removed"
} catch { Chk "uninstall-clears-legacy-rule" $false $_.Exception.Message }

$fails = ($res | Select-String '^FAIL').Count
Note ("=== DONE  fails=$fails ===")
$res | Out-File -FilePath $out -Encoding UTF8
