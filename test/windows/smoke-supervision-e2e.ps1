# Full supervision + stop semantics, with the real OpenProcess liveness check.
$Shared='\\host.lan\Data'; $Out="$Shared\final-result.txt"
$WorkDir='C:\unarr'; $DataDir="$env:LOCALAPPDATA\unarr"
$pass=0;$fail=0
function Say($m){ $l="$(Get-Date -Format 'HH:mm:ss')  $m"; Write-Host $l; Add-Content $Out $l }
function Check($ok,$m){ if($ok){$script:pass++;Say "  [PASS] $m"}else{$script:fail++;Say "  [FAIL] $m"} }
function Pids{ @(Get-Process unarr -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq "$WorkDir\unarr.exe" } | Select-Object -Expand Id) }
function StateHasPid([int]$want){
  if(-not (Test-Path "$DataDir\daemon.state.json")){ return $false }
  try { return ((Get-Content "$DataDir\daemon.state.json" -Raw) -match "`"pid`":\s*$want") } catch { return $false }
}
function WaitFor([scriptblock]$c,[int]$s,[string]$w){ Say "  waiting up to ${s}s for $w ..."
  $sw=[System.Diagnostics.Stopwatch]::StartNew()
  while($sw.Elapsed.TotalSeconds -lt $s){ if(& $c){return [math]::Round($sw.Elapsed.TotalSeconds)}; Start-Sleep -Milliseconds 500 }; return -1 }

Remove-Item $Out -ErrorAction SilentlyContinue
Say "=== FINAL: supervision + stop semantics on real Windows ==="
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
Check ((WaitFor { StateHasPid $p1 } 90 "registration") -ge 0) "daemon registered (state names pid $p1)"

Say "[A] crash -> the shim must bring it back"
taskkill /pid $p1 /f 2>&1 | Out-Null
Start-Sleep -Seconds 2
Check (Test-Path "$DataDir\daemon.state.json") "state file SURVIVES a crash (real crashes still reported)"
Check (-not (Test-Path "$DataDir\daemon.stopped")) "a crash records NO stop intent"
$t=WaitFor { $n=Pids; $n.Count -gt 0 -and $n[0] -ne $p1 } 120 "the shim to relaunch"
Check ($t -ge 0) "daemon RESPAWNED after the crash (${t}s)"
$p2=if((Pids).Count -gt 0){(Pids)[0]}else{0}
Check ((WaitFor { StateHasPid $p2 } 120 "re-registration") -ge 0) "respawned daemon registered (state names pid $p2)"

Say "[B] deliberate stop -> must stop, stay stopped, and leave no wreckage"
& "$WorkDir\unarr.exe" stop 2>&1 | Out-Null
Start-Sleep -Seconds 5
Check ((Pids).Count -eq 0) "daemon is down after stop"
Check (Test-Path "$DataDir\daemon.stopped") "stop recorded the intent marker"
Check (-not (Test-Path "$DataDir\daemon.state.json")) "clean stop left NO state file (no false crash report)"
Check ((WaitFor { (Pids).Count -gt 0 } 90 "a (wrong) resurrection") -lt 0) "daemon STAYED down"

Say "[C] restart clears the stop intent"
schtasks /run /tn unarr 2>&1 | Out-Null
Check ((WaitFor { (Pids).Count -gt 0 } 120 "the daemon to start again") -ge 0) "daemon starts again after a stop"
Check ((WaitFor { -not (Test-Path "$DataDir\daemon.stopped") } 60 "the marker to be consumed") -ge 0) "startup consumed the stop marker"

& "$WorkDir\unarr.exe" stop 2>&1 | Out-Null
& "$WorkDir\unarr.exe" daemon uninstall 2>&1 | Out-Null
Say "RESULT passed=$pass failed=$fail"
Say "DONE"
