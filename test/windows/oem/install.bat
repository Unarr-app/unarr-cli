@echo off
REM Runs ONCE, automatically, at the end of the unattended Windows install
REM (dockurr copies this dir to C:\OEM and executes install.bat via RunOnce).
REM Two jobs: (1) enable WinRM so the host can drive the guest headlessly on
REM every future run, (2) run the unarr smoke checks and drop the result on the
REM shared \\host.lan\Data volume so the host sees PASS/FAIL with no interaction.
echo [oem] unarr Windows test bootstrap starting >> C:\OEM\install.log

REM --- Enable WinRM (HTTP, Basic auth, allow unencrypted for the test VM) ---
REM Test-only VM on an isolated NAT — plain HTTP/Basic is fine here.
powershell -NoProfile -ExecutionPolicy Bypass -Command ^
  "Set-NetConnectionProfile -NetworkCategory Private -ErrorAction SilentlyContinue; ^
   winrm quickconfig -quiet -force; ^
   winrm set winrm/config/service '@{AllowUnencrypted=\"true\"}'; ^
   winrm set winrm/config/service/auth '@{Basic=\"true\"}'; ^
   Enable-PSRemoting -Force -SkipNetworkProfileCheck; ^
   New-NetFirewallRule -Name unarr-winrm -DisplayName 'WinRM 5985' -Enabled True -Direction Inbound -Protocol TCP -LocalPort 5985 -Action Allow -ErrorAction SilentlyContinue" ^
  >> C:\OEM\install.log 2>&1

echo [oem] WinRM configured >> C:\OEM\install.log

REM --- Run the smoke checks now, capture result to the shared volume ---
REM \\host.lan\Data is the ./shared dir on the host. smoke.ps1 lives there.
powershell -NoProfile -ExecutionPolicy Bypass -File "\\host.lan\Data\smoke.ps1" ^
  > "\\host.lan\Data\smoke-result.txt" 2>&1
echo [oem] smoke.ps1 exit=%ERRORLEVEL% >> C:\OEM\install.log
echo EXIT %ERRORLEVEL% >> "\\host.lan\Data\smoke-result.txt"

echo [oem] done >> C:\OEM\install.log
