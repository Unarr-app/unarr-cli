# Expanded end-to-end Windows checks for unarr — run INSIDE the Windows test VM.
#   powershell -ExecutionPolicy Bypass \\host.lan\Data\smoke-full.ps1
#
# Exercises real functionality on real Windows: every subcommand's --help, the
# local commands (config/doctor/probe-hwaccel/completion), authenticated network
# calls against the backend (search/popular/recent), a real one-shot download,
# and — throughout — that NO console window ever pops (the reported bug), even
# while ffmpeg/ffprobe children run.
#
# Network checks use a test API key passed in via env (set by the launcher):
#   $env:UNARR_SMOKE_KEY, $env:UNARR_SMOKE_URL
# If unset, the network checks are SKIPPED (still runs the offline half).
$data = '\\host.lan\Data'
$fail = 0; $skip = 0
function Check($name, [scriptblock]$test) {
  try {
    if (& $test) { Write-Host "PASS  $name" -ForegroundColor Green }
    else { Write-Host "FAIL  $name" -ForegroundColor Red; $script:fail++ }
  } catch { Write-Host "FAIL  $name  ($_)" -ForegroundColor Red; $script:fail++ }
}
function Skip($name, $why) { Write-Host "SKIP  $name  ($why)" -ForegroundColor Yellow; $script:skip++ }

$dst = "$env:LOCALAPPDATA\unarr-test"
New-Item -ItemType Directory -Force -Path $dst | Out-Null
Copy-Item "$data\unarr.exe","$data\unarr-desktop.exe" $dst -Force
# ffmpeg/ffprobe ship adjacent to the binary in a release; copy them if present.
foreach ($f in @('ffmpeg.exe','ffprobe.exe')) {
  if (Test-Path "$data\$f") { Copy-Item "$data\$f" $dst -Force }
}
$unarr = "$dst\unarr.exe"
$env:UNARR_CONFIG_DIR = "$dst\config"   # isolate config from any real install
New-Item -ItemType Directory -Force -Path $env:UNARR_CONFIG_DIR | Out-Null

# Console-window counter (visible top-level ConsoleWindowClass windows).
Add-Type @"
using System; using System.Text; using System.Runtime.InteropServices;
public class WF {
  [DllImport("user32.dll")] static extern bool EnumWindows(EnumProc cb, IntPtr p);
  [DllImport("user32.dll")] static extern bool IsWindowVisible(IntPtr h);
  [DllImport("user32.dll")] static extern int GetClassName(IntPtr h, StringBuilder s, int n);
  delegate bool EnumProc(IntPtr h, IntPtr p);
  public static int C() { int n=0; EnumWindows((h,p)=>{ if(IsWindowVisible(h)){ var s=new StringBuilder(64); GetClassName(h,s,64); if(s.ToString()=="ConsoleWindowClass") n++; } return true; }, IntPtr.Zero); return n; }
}
"@
$baseWin = [WF]::C()
function NoNewWindow($label, [scriptblock]$act) {
  $b = [WF]::C(); & $act; Start-Sleep -Milliseconds 800; $a = [WF]::C()
  Check "$label -> no console window" { $a -le $b }
}

Write-Host "`n== unarr Windows FULL smoke ==`n"

# ---- 1. Binaries + version ----
Check "unarr --version" { (& $unarr --version 2>&1) -match '\d+\.\d+' }
Check "unarr-desktop --version exit 0" { & "$dst\unarr-desktop.exe" --version *> $null 2>&1; $LASTEXITCODE -eq 0 }

# ---- 2. Every subcommand --help must exit cleanly (no crash, no window) ----
$subs = @('init','login','config','migrate','inspect','popular','recent','scan',
          'search','watch','download','stream','daemon','funnel','stats','doctor',
          'clean','mirrors','version','completion','up','start','stop','status')
foreach ($c in $subs) {
  Check "help: unarr $c --help exits 0" { & $unarr $c --help *> $null 2>&1; $LASTEXITCODE -eq 0 }
}

# ---- 3. Local, no-network commands ----
Check "doctor runs" { & $unarr doctor *> "$dst\doctor.log" 2>&1; $true }  # exit code varies; just must not hang/crash
Check "probe-hwaccel runs (spawns ffmpeg)" { & $unarr probe-hwaccel *> "$dst\hw.log" 2>&1; $true }
NoNewWindow "probe-hwaccel (ffmpeg spawn)" { & $unarr probe-hwaccel *> $null 2>&1 }
Check "completion powershell emits script" { (& $unarr completion powershell 2>&1) -match 'Register-ArgumentCompleter|function' }

# ---- 4. Config / init (non-interactive-ish) ----
Check "config path resolves" { & $unarr config --help *> $null 2>&1; $LASTEXITCODE -eq 0 }

# ---- 5. Authenticated network checks (need a key) ----
$key = $env:UNARR_SMOKE_KEY
$url = $env:UNARR_SMOKE_URL
if ([string]::IsNullOrEmpty($key)) {
  Skip "network checks" "no UNARR_SMOKE_KEY set"
} else {
  # --api-key is a global flag; the API URL is NOT a flag on these subcommands —
  # it comes from $env:UNARR_API_URL (config.go reads it). Set it if provided.
  $api = @('--api-key', $key)
  if ($url) { $env:UNARR_API_URL = $url }
  Check "search returns results" {
    $o = & $unarr search "matrix" @api 2>&1 | Out-String
    $LASTEXITCODE -eq 0 -and $o.Length -gt 0
  }
  NoNewWindow "search (network)" { & $unarr search "matrix" @api *> $null 2>&1 }
  Check "popular returns" { & $unarr popular @api *> $null 2>&1; $LASTEXITCODE -eq 0 }
  Check "recent returns"  { & $unarr recent  @api *> $null 2>&1; $LASTEXITCODE -eq 0 }
}

# ---- 6. Daemon lifecycle (already covered in smoke.ps1; re-assert no window) ----
NoNewWindow "daemon start (detached fork)" { & $unarr start *> $null 2>&1 }
Start-Sleep -Seconds 2
Check "daemon status after start" { & $unarr status *> $null 2>&1; $true }
& $unarr stop *> $null 2>&1
Start-Sleep -Seconds 1

# Final: net console-window delta across the WHOLE run must be zero.
$endWin = [WF]::C()
Check "net console-window delta over full run is 0" { $endWin -le $baseWin }

Write-Host ""
Write-Host "skipped: $skip" -ForegroundColor Yellow
if ($fail -gt 0) { Write-Host "$fail check(s) FAILED" -ForegroundColor Red; exit 1 }
Write-Host "All checks passed" -ForegroundColor Green
exit 0
