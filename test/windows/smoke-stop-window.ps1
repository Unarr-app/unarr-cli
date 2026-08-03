# The startup-window gap: stop the daemon DURING the window between a shim
# relaunch and that daemon registering, while the state file still names the
# previous, dead PID. Before the fix this killed nothing and left a live daemon.
$Shared='\\host.lan\Data'; $Out="$Shared\window-result.txt"
$WorkDir='C:\unarr'; $DataDir="$env:LOCALAPPDATA\unarr"
$pass=0;$fail=0
function Say($m){ $l="$(Get-Date -Format 'HH:mm:ss')  $m"; Write-Host $l; Add-Content $Out $l }
function Check($ok,$m){ if($ok){$script:pass++;Say "  [PASS] $m"}else{$script:fail++;Say "  [FAIL] $m"} }
function Pids{ @(Get-Process unarr -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq "$WorkDir\unarr.exe" } | Select-Object -Expand Id) }
function StatePid{ if(-not(Test-Path "$DataDir\daemon.state.json")){return -1}
  try{ if((Get-Content "$DataDir\daemon.state.json" -Raw) -match '"pid":\s*(\d+)'){return [int]$Matches[1]} }catch{}; return -1 }
function WaitFor([scriptblock]$c,[int]$s,[string]$w){ Say "  waiting up to ${s}s for $w ..."
  $sw=[System.Diagnostics.Stopwatch]::StartNew()
  while($sw.Elapsed.TotalSeconds -lt $s){ if(& $c){return [math]::Round($sw.Elapsed.TotalSeconds)}; Start-Sleep -Milliseconds 200 }; return -1 }

Remove-Item $Out -ErrorAction SilentlyContinue
Say "=== stop during the post-respawn window ==="
Copy-Item "$Shared\unarr.exe" $WorkDir -Force
& "$WorkDir\unarr.exe" daemon uninstall 2>&1 | Out-Null
Get-Process unarr -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
Remove-Item "$DataDir\daemon.state.json","$DataDir\daemon.stopped" -ErrorAction SilentlyContinue
Start-Sleep -Seconds 3
& "$WorkDir\unarr.exe" daemon install 2>&1 | Out-Null
Start-Sleep -Seconds 3
schtasks /run /tn unarr 2>&1 | Out-Null

Check ((WaitFor { (Pids).Count -gt 0 } 120 "the daemon to start") -ge 0) "daemon started"
$p1=(Pids)[0]
Check ((WaitFor { (StatePid) -eq $p1 } 90 "it to register") -ge 0) "daemon registered as pid $p1"

Say "[A] crash, then stop IMMEDIATELY - inside the window"
taskkill /pid $p1 /f 2>&1 | Out-Null
$t=WaitFor { $n=Pids; $n.Count -gt 0 -and $n[0] -ne $p1 } 120 "the shim to relaunch"
Check ($t -ge 0) "daemon respawned (${t}s)"
$p2=(Pids)[0]
Say "  respawned pid=$p2 ; state file still names pid=$(StatePid)"
Check ((StatePid) -ne $p2) "we ARE inside the window (state file does not name the live daemon yet)"

Say "  stopping now"
& "$WorkDir\unarr.exe" stop 2>&1 | Out-Null
$t=WaitFor { (Pids).Count -eq 0 } 60 "the live daemon to shut down"
Check ($t -ge 0) "the LIVE daemon was stopped despite the stale state file (${t}s)"
Check (Test-Path "$DataDir\daemon.stopped") "stop recorded the intent marker"
Check ((WaitFor { -not (Test-Path "$DataDir\daemon.state.json") } 30 "the state file to be reaped") -ge 0) "no state file left behind (no false crash report)"
Check ((WaitFor { (Pids).Count -gt 0 } 90 "a (wrong) resurrection") -lt 0) "daemon STAYED down"

Say "[B] no regression: normal start/crash/respawn still works"
schtasks /run /tn unarr 2>&1 | Out-Null
Check ((WaitFor { (Pids).Count -gt 0 } 120 "restart") -ge 0) "daemon starts again after the stop"
Check ((WaitFor { -not (Test-Path "$DataDir\daemon.stopped") } 60 "marker consumption") -ge 0) "startup consumed the stop marker"
$p3=(Pids)[0]
Check ((WaitFor { (StatePid) -eq $p3 } 90 "registration") -ge 0) "registered as pid $p3"
taskkill /pid $p3 /f 2>&1 | Out-Null
Check ((WaitFor { $n=Pids; $n.Count -gt 0 -and $n[0] -ne $p3 } 120 "respawn") -ge 0) "still respawns after a crash"

Say "[C] no regression: stop of a fully-registered daemon"
$p4=(Pids)[0]
Check ((WaitFor { (StatePid) -eq $p4 } 90 "registration") -ge 0) "registered as pid $p4"
& "$WorkDir\unarr.exe" stop 2>&1 | Out-Null
$t=WaitFor { (Pids).Count -eq 0 } 60 "the daemon to shut down"
Check ($t -ge 0) "registered daemon stops (${t}s)"
Check ((WaitFor { -not (Test-Path "$DataDir\daemon.state.json") } 30 "the state file to be reaped") -ge 0) "no state file after a clean stop"
Check ((WaitFor { (Pids).Count -gt 0 } 60 "a (wrong) resurrection") -lt 0) "stays down"

& "$WorkDir\unarr.exe" daemon uninstall 2>&1 | Out-Null
Say "RESULT passed=$pass failed=$fail"
Say "DONE"
