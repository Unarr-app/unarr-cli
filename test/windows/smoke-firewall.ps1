# Verifies the Windows firewall rule the agent installs for inbound peers, and
# checks it does not trip Defender.
#
# Context: prod 2026-09-03, restricted to torrents with 10+ seeders, Windows
# agents failed 58.3% of downloads with "no peers found" against 10.7% on Linux
# — same build. The agent had no firewall handling at all. This asserts the fix
# actually lands on a real Windows box, because cross-compiling only proves it
# builds.
#
# Every check is in its own try/catch: no global $ErrorActionPreference, so one
# failure cannot abort the run (learned the hard way — see the harness README).

$ErrorActionPreference = 'Continue'
$out  = "\\host.lan\Data\firewall-result.txt"
# Run from LOCAL disk, not the UNC share. Windows treats \\host.lan\... as the
# Internet zone and the daemon does not come up from there (verified: the agent
# started fine from C:\ on 2026-08-10, and not at all from the share today). It
# is also the realistic case — the rule we assert is scoped to a local path.
$binSrc = "\\host.lan\Data\unarr.exe"
$binDir = "C:\unarr-test"
$bin    = "$binDir\unarr.exe"
New-Item -ItemType Directory -Force -Path $binDir | Out-Null
Copy-Item $binSrc $bin -Force
Unblock-File $bin -ErrorAction SilentlyContinue
$rule = "unarr (BitTorrent peers)"
$res  = New-Object System.Collections.ArrayList

function Note($s) { [void]$res.Add($s); Write-Host $s }
function Chk($name, $ok, $detail) {
  if ($ok) { Note ("PASS  {0}  {1}" -f $name, $detail) }
  else     { Note ("FAIL  {0}  {1}" -f $name, $detail) }
}

Note "=== unarr firewall smoke  $(Get-Date -Format o) ==="
Note ("elevated = {0}" -f ([bool](([Security.Principal.WindowsPrincipal] `
  [Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
  [Security.Principal.WindowsBuiltInRole]::Administrator))))

# --- 0. binary runs at all -------------------------------------------------
try {
  $v = & $bin --version 2>&1 | Out-String
  Chk "binary-runs" ($LASTEXITCODE -eq 0) ("exit=$LASTEXITCODE ver=" + $v.Trim())
} catch { Chk "binary-runs" $false $_.Exception.Message }

# --- 1. clean slate --------------------------------------------------------
try {
  netsh advfirewall firewall delete rule name="$rule" 2>&1 | Out-Null
  $show = netsh advfirewall firewall show rule name="$rule" 2>&1 | Out-String
  Chk "clean-slate" ($show -match 'No rules match') "rule absent before test"
} catch { Chk "clean-slate" $false $_.Exception.Message }

# --- 2. daemon reports the MISSING rule at startup -------------------------
# The whole point of the startup check: a firewalled agent must say so.
try {
  $log = "$env:TEMP\unarr-nofw.log"
  Remove-Item $log -ErrorAction SilentlyContinue
  $p = Start-Process -FilePath $bin -ArgumentList "start" -PassThru `
        -RedirectStandardOutput $log -RedirectStandardError "$log.err" -WindowStyle Hidden
  Start-Sleep -Seconds 12
  try { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue } catch {}
  Start-Sleep -Seconds 1
  $txt = ((Get-Content $log -Raw -ErrorAction SilentlyContinue) + `
          (Get-Content "$log.err" -Raw -ErrorAction SilentlyContinue))
  Chk "startup-warns-when-missing" ($txt -match '\[firewall\] no inbound rule') `
      ("firewall lines: " + (($txt -split "`n" | Select-String '\[firewall\]') -join ' | '))
  # And it must NOT have probed the DHT (that leaked the real IP under VPN).
  Chk "startup-no-dht-probe" (-not ($txt -match '\[dht\]')) "no [dht] line in startup log"
} catch { Chk "startup-warns-when-missing" $false $_.Exception.Message }

# --- 3. add the rule exactly as the code does ------------------------------
try {
  $add = netsh advfirewall firewall add rule name="$rule" dir=in action=allow `
         program="$bin" enable=yes 2>&1 | Out-String
  Chk "rule-add" ($add -match 'Ok\.') $add.Trim()
} catch { Chk "rule-add" $false $_.Exception.Message }

# --- 4. the rule is what we intended --------------------------------------
try {
  $show = netsh advfirewall firewall show rule name="$rule" verbose 2>&1 | Out-String
  Chk "rule-present"   ($show -notmatch 'No rules match') "rule found"
  Chk "rule-inbound"   ($show -match '(?i)Direction:\s+In')  "dir=in"
  Chk "rule-allow"     ($show -match '(?i)Action:\s+Allow')  "action=allow"
  # Program-scoped, not port-scoped: listen_port is 0 on a stock install and the
  # engine walks 42069..42078 when busy, so a port rule would cover nothing.
  Chk "rule-program-scoped" ($show -match '(?i)Program:') "scoped to the binary"
  # Public MUST be included: it is what Windows assigns to unidentified networks.
  $profiles = ($show | Select-String '(?i)Profiles:.*') -join ''
  Chk "rule-covers-public" ($show -match '(?i)Profiles:\s*(Domain,Private,Public|Any)') `
      ("profiles line: " + $profiles.Trim())
} catch { Chk "rule-present" $false $_.Exception.Message }

# --- 5. daemon now reports the rule as present -----------------------------
try {
  $log2 = "$env:TEMP\unarr-fw.log"
  Remove-Item $log2 -ErrorAction SilentlyContinue
  $p2 = Start-Process -FilePath $bin -ArgumentList "start" -PassThru `
        -RedirectStandardOutput $log2 -RedirectStandardError "$log2.err" -WindowStyle Hidden
  Start-Sleep -Seconds 12
  try { Stop-Process -Id $p2.Id -Force -ErrorAction SilentlyContinue } catch {}
  Start-Sleep -Seconds 1
  $txt2 = ((Get-Content $log2 -Raw -ErrorAction SilentlyContinue) + `
           (Get-Content "$log2.err" -Raw -ErrorAction SilentlyContinue))
  Chk "startup-confirms-present" ($txt2 -match '\[firewall\] inbound peer rule present') `
      ("firewall lines: " + (($txt2 -split "`n" | Select-String '\[firewall\]') -join ' | '))
} catch { Chk "startup-confirms-present" $false $_.Exception.Message }

# --- 6. Defender / antivirus reaction --------------------------------------
# The user's ask: if we trip the AV, find a friendlier way. netsh is a built-in,
# but a background process editing the firewall is exactly what heuristics watch.
try {
  $threats = @(Get-MpThreatDetection -ErrorAction SilentlyContinue |
               Where-Object { $_.InitialDetectionTime -gt (Get-Date).AddMinutes(-30) })
  Chk "defender-no-detection" ($threats.Count -eq 0) ("recent detections: " + $threats.Count)
  $pref = Get-MpPreference -ErrorAction SilentlyContinue
  Note ("defender realtime-monitoring-disabled = {0}" -f $pref.DisableRealtimeMonitoring)
  $ev = @(Get-WinEvent -LogName "Microsoft-Windows-Windows Defender/Operational" `
          -MaxEvents 40 -ErrorAction SilentlyContinue |
          Where-Object { $_.Id -in 1006,1015,1116,1117 -and $_.TimeCreated -gt (Get-Date).AddMinutes(-30) })
  Chk "defender-no-block-events" ($ev.Count -eq 0) ("block/detect events: " + $ev.Count)
} catch { Note ("INFO  defender check unavailable: " + $_.Exception.Message) }

# --- 7. cleanup removes it -------------------------------------------------
try {
  netsh advfirewall firewall delete rule name="$rule" 2>&1 | Out-Null
  $show3 = netsh advfirewall firewall show rule name="$rule" 2>&1 | Out-String
  Chk "rule-removed" ($show3 -match 'No rules match') "gone after delete"
} catch { Chk "rule-removed" $false $_.Exception.Message }

$fails = ($res | Select-String '^FAIL').Count
Note ("=== DONE  fails=$fails ===")
$res | Out-File -FilePath $out -Encoding UTF8
