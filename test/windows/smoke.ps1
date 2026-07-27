# Automated Windows smoke checks for unarr — run INSIDE the Windows test VM.
#   powershell -ExecutionPolicy Bypass \\host.lan\Data\smoke.ps1
#
# Verifies the things cross-compilation cannot: console-window suppression on
# spawned children, and that the scheduled-task autostart registers with the
# reliability settings. Prints PASS/FAIL per check; exits non-zero on any FAIL.
#
# NOTE: no global $ErrorActionPreference='Stop' — each check is isolated in
# Check{} so one failing probe never aborts the rest of the run.
$data = '\\host.lan\Data'
$fail = 0
function Check($name, [scriptblock]$test) {
  try {
    if (& $test) { Write-Host "PASS  $name" -ForegroundColor Green }
    else { Write-Host "FAIL  $name" -ForegroundColor Red; $script:fail++ }
  } catch { Write-Host "FAIL  $name  ($_)" -ForegroundColor Red; $script:fail++ }
}

# Stage the binaries locally (UNC exec can behave oddly).
$dst = "$env:LOCALAPPDATA\unarr-test"
New-Item -ItemType Directory -Force -Path $dst | Out-Null
Copy-Item "$data\unarr.exe","$data\unarr-desktop.exe" $dst -Force
$unarr   = "$dst\unarr.exe"
$desktop = "$dst\unarr-desktop.exe"

Write-Host "`n== unarr Windows smoke ==`n"

# 1. Binaries run.
# unarr.exe is a console binary → --version prints to stdout, capture it.
Check "unarr --version prints a version" { (& $unarr --version 2>&1) -match '\d+\.\d+' }
# unarr-desktop.exe is -H=windowsgui → NO console, so --version cannot write to
# a captured stdout. It's still expected to exit 0 (that's the self-updater's
# own smoke test). Assert the EXIT CODE, not the output.
Check "unarr-desktop --version exits 0 (GUI binary, no stdout)" {
  & $desktop --version *> $null 2>&1; $LASTEXITCODE -eq 0
}

# Win32 helper: enumerate VISIBLE top-level console windows (class
# ConsoleWindowClass). This is the ground truth for "did a console window pop".
Add-Type @"
using System;
using System.Text;
using System.Collections.Generic;
using System.Runtime.InteropServices;
public class W {
  [DllImport("user32.dll")] static extern bool EnumWindows(EnumProc cb, IntPtr p);
  [DllImport("user32.dll")] static extern bool IsWindowVisible(IntPtr h);
  [DllImport("user32.dll")] static extern int GetClassName(IntPtr h, StringBuilder s, int n);
  delegate bool EnumProc(IntPtr h, IntPtr p);
  public static int ConsoleWindows() {
    int n = 0;
    EnumWindows((h, p) => {
      if (IsWindowVisible(h)) {
        var sb = new StringBuilder(64);
        GetClassName(h, sb, 64);
        if (sb.ToString() == "ConsoleWindowClass") n++;
      }
      return true;
    }, IntPtr.Zero);
    return n;
  }
}
"@

# 2. CONSOLE-WINDOW SUPPRESSION — the reported bug.
# The daemon (unarr.exe, a console binary) is what the tray spawns and what
# flashed a window. `unarr start` forks the daemon via detachedSysProcAttr()
# (DETACHED_PROCESS|CREATE_NEW_PROCESS_GROUP|CREATE_NO_WINDOW). With the fix, the
# forked daemon must NOT add a visible console window. We measure the count of
# visible ConsoleWindowClass windows before/after a start, then stop it.
$before = [W]::ConsoleWindows()
& $unarr start *> $null 2>&1        # forks the detached daemon
Start-Sleep -Seconds 3
$after = [W]::ConsoleWindows()
& $unarr stop  *> $null 2>&1
Start-Sleep -Seconds 1
Check "no new console window when the daemon is started" { $after -le $before }

# 3. SCHEDULED-TASK AUTOSTART with the reliability settings.
& $unarr daemon install *> "$dst\install.log" 2>&1
Check "scheduled task 'unarr' exists" {
  schtasks /query /tn unarr *> $null; $LASTEXITCODE -eq 0
}
$taskXml = (schtasks /query /tn unarr /xml 2>$null) -join "`n"
Check "task has a logon Delay (network-race fix)"       { $taskXml -match '<Delay>PT\d+S</Delay>' }
Check "task has RestartOnFailure (supervisor)"          { $taskXml -match '<RestartOnFailure>' }
Check "task has StartWhenAvailable"                     { $taskXml -match '<StartWhenAvailable>true</StartWhenAvailable>' }
Check "task does NOT use Start-Transcript -NoClobber"   { $taskXml -notmatch '-NoClobber' }

# Cleanup so we don't leave the task/daemon around.
& $unarr daemon uninstall *> $null 2>&1
schtasks /delete /tn unarr /f *> $null 2>&1

Write-Host ""
if ($fail -gt 0) { Write-Host "$fail check(s) FAILED" -ForegroundColor Red; exit 1 }
Write-Host "All checks passed" -ForegroundColor Green
exit 0
