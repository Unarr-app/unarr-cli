# smoke-boottime.ps1 - real-Windows verification for the crash-report fixes.
#
# Two things cross-compilation cannot prove, and this can:
#
#   1. sysinfo.BootTime() calls GetTickCount64 through a lazy kernel32 binding.
#      That binding either resolves on a real Windows or it does not, and the
#      value it yields either matches the boot instant the OS reports or it does
#      not. Compared here against Win32_OperatingSystem.LastBootUpTime.
#   2. The package tests for agent (StateFromPreviousBoot) and unarr-desktop
#      (readStatus reaping a pre-boot state file) run against the real
#      LOCALAPPDATA layout and the real process-liveness syscall
#      (OpenProcess/GetExitCodeProcess), not the unix stand-ins.
#   3. sysinfo.LastShutdown() reads a FILETIME out of the real registry
#      (HKLM\SYSTEM\CurrentControlSet\Control\Windows\ShutdownTime) and parses it
#      to the instant Windows itself logged the shutdown (System event 6006).
#      That value is the ONLY signal that can see a Fast Startup power cycle:
#      a hybrid shutdown hibernates the kernel session, so GetTickCount64 - and
#      with it BootTime() - carries straight over it, and a state file the
#      shutdown killed looks NEWER than the boot. Section 6 also reports whether
#      this machine is in that state right now.
#
# ASCII ONLY, deliberately: Windows PowerShell 5.1 reads a BOM-less file through
# the system ANSI code page, so a UTF-8 em dash inside a STRING literal decodes
# to three CP1252 characters - one of which is a double quote, which terminates
# the string early and makes the whole script a parse error. (Measured on the VM
# harness. It is the same encoding fault that mangles em dashes in the daemon's
# own log lines.) In a # comment it is harmless; in a string it is fatal.
#
# Run:  powershell -ExecutionPolicy Bypass \\host.lan\Data\smoke-boottime.ps1
# Result file: \\host.lan\Data\boottime-result.txt

$ErrorActionPreference = 'Continue'
$share = '\\host.lan\Data'
$out = Join-Path $share 'boottime-result.txt'
$lines = @()
$fail = 0

function Say($m) { $script:lines += $m; Write-Host $m }

Say "=== unarr boot-time / crash-detection smoke ==="
Say "host: $env:COMPUTERNAME  user: $env:USERNAME"

# --- 1. GetTickCount64 vs what Windows says --------------------------------
$osBoot = (Get-CimInstance Win32_OperatingSystem).LastBootUpTime.ToUniversalTime()
Say "windows LastBootUpTime: $($osBoot.ToString('o'))"

# The flag is QUOTED: PowerShell parses a bare -test.v as its own parameter and
# hands the exe just "-test", which the Go test binary rejects.
$sysOut = & (Join-Path $share 'sysinfo_test.exe') '-test.v' 2>&1 | Out-String
$m = [regex]::Match($sysOut, 'boot=(\S+)')
if (-not $m.Success) {
    Say "FAIL: sysinfo_test.exe did not report a boot instant"
    Say $sysOut
    $fail++
} else {
    $goBoot = [datetime]::Parse($m.Groups[1].Value).ToUniversalTime()
    $delta = [math]::Abs(($goBoot - $osBoot).TotalSeconds)
    Say "sysinfo.BootTime():     $($goBoot.ToString('o'))  (delta ${delta}s)"
    # A couple of minutes of slack: LastBootUpTime is stamped a moment after the
    # kernel starts ticking, and the guest clock resyncs against the host.
    if ($delta -gt 120) {
        Say "FAIL: GetTickCount64-derived boot is ${delta}s from the OS value"
        $fail++
    } else {
        Say "PASS: boot instant agrees with Windows"
    }
}

if ($sysOut -match 'no boot-time source') {
    Say "FAIL: BootTime() reported no source ON WINDOWS - the kernel32 binding did not resolve"
    $fail++
}

# --- 2. the package tests, on real Windows ---------------------------------
foreach ($exe in @('sysinfo_test.exe', 'agent_test.exe', 'desktop_test.exe')) {
    $path = Join-Path $share $exe
    if (-not (Test-Path $path)) { Say "SKIP: $exe not deployed"; continue }
    # -test.short skips the one test that would shell out to `go build`; there is
    # no toolchain in the guest.
    #
    # TestDiskInfoBounded is skipped for the same class of reason: it is an AST
    # guard that reads its own package's .go sources from the working directory,
    # and a `go test -c` binary run from the share has no sources next to it. It
    # is not a Windows finding, and letting it fail here would train the reader
    # to ignore a red line.
    $o = & $path '-test.short' '-test.skip=TestDiskInfoBoundedIsUsedByRegister' 2>&1 | Out-String
    if ($LASTEXITCODE -eq 0) {
        Say "PASS: $exe"
    } else {
        Say "FAIL: $exe (exit $LASTEXITCODE)"
        Say $o
        $fail++
    }
}

# --- 3. the crash-detection tests by name, verbosely ------------------------
# The pass above is a whole-package roll-up, and this package has a test that
# fails for an unrelated reason in this harness (it reads its own .go source,
# which is not deployed next to the exe). So the tests this change is actually
# about are named and asserted individually.
$named = @(
    @{ exe = 'agent_test.exe';   run = 'PreviousBoot'; want = 4 },
    @{ exe = 'desktop_test.exe'; run = 'ReadStatus';   want = 5 }
)
foreach ($n in $named) {
    $path = Join-Path $share $n.exe
    if (-not (Test-Path $path)) { Say "SKIP: $($n.exe) not deployed"; continue }
    $o = & $path '-test.v' '-test.short' "-test.run=$($n.run)" 2>&1 | Out-String
    $passes = ([regex]::Matches($o, '(?m)^--- PASS')).Count
    $fails = ([regex]::Matches($o, '(?m)^--- FAIL')).Count
    Say "$($n.exe) -test.run=$($n.run): $passes pass, $fails fail (want >= $($n.want) pass, 0 fail)"
    foreach ($line in ($o -split "`r?`n")) {
        if ($line -match '^\s*(---|===) (PASS|FAIL|SKIP)') { Say "    $line" }
    }
    if ($fails -gt 0 -or $passes -lt $n.want) {
        Say "FAIL: $($n.exe) crash-detection tests did not all pass on real Windows"
        Say $o
        $fail++
    } else {
        Say "PASS: $($n.exe) crash-detection tests"
    }
}

# --- 4. the crash-report log collection, against the REAL CLI ---------------
# The bug this whole change exists for lives on Windows, and the piece that can
# only be proven here is the argv: unarr-desktop shells out to
# `unarr daemon logs --boot` to collect the file a Go panic lands in. A CLI that
# answers "unknown flag" makes the tray silently collect one log instead of two,
# and the tray SWALLOWS that failure by design (a missing boot log is an
# ordinary state of the world) - so nothing but this would notice.
#
# Needs unarr.exe deployed next to desktop_test.exe, which run.sh already does:
# the test resolves a CLI from its own directory when there is no Go toolchain.
# RUN FROM A LOCAL DIRECTORY, NOT THE SHARE. A process started from
# \\host.lan\Data inherits a UNC working directory, and every child it then
# spawns fails: measured here as exit status 1, zero bytes of output and ~72s of
# SMB timeout PER CALL - even for `unarr version`, which touches no files. That
# is an artefact of the harness, not of the product (installers put both
# binaries under %LOCALAPPDATA%), and it is why the other smokes copy to
# C:\unarr first. The CLI has to land in the SAME directory: the e2e resolves
# it as a sibling of the test binary when there is no Go toolchain.
$localBin = 'C:\unarrtest'
New-Item -ItemType Directory -Force -Path $localBin | Out-Null
Copy-Item (Join-Path $share 'desktop_test.exe') $localBin -Force -ErrorAction SilentlyContinue
Copy-Item (Join-Path $share 'unarr.exe') $localBin -Force -ErrorAction SilentlyContinue
$dt = Join-Path $localBin 'desktop_test.exe'
if (Test-Path $dt) {
    Push-Location $localBin
    $o = & $dt '-test.v' '-test.run=TestReportLogsAgainstTheRealCLI' 2>&1 | Out-String
    Pop-Location
    foreach ($line in ($o -split "`r?`n")) {
        if ($line -match '^\s*(---|===) (PASS|FAIL|SKIP)|using the CLI') { Say "    $line" }
    }
    if ($o -match '(?m)^--- PASS') {
        Say "PASS: `unarr daemon logs --boot` feeds the crash report on real Windows"
    } elseif ($o -match '(?m)^--- SKIP') {
        Say "SKIP: the e2e could not find a CLI to test against (deploy unarr.exe next to desktop_test.exe)"
    } else {
        Say "FAIL: the crash-report log collection did not work against the real CLI"
        Say $o
        $fail++
    }
} else {
    Say "SKIP: desktop_test.exe not deployed"
}

# --- 5. the boot log really does hold a panic -------------------------------
# Direct, no Go involved: write a panic into unarr.boot.log and ask the shipped
# CLI for it exactly the way the tray does.
$dataDir = Join-Path $env:LOCALAPPDATA 'unarr'
New-Item -ItemType Directory -Force -Path $dataDir | Out-Null
$bootLog = Join-Path $dataDir 'unarr.boot.log'
$marker = "panic: smoke-boottime marker $(Get-Random)"
Add-Content -Path $bootLog -Value $marker -Encoding UTF8
$bootOut = & (Join-Path $localBin 'unarr.exe') daemon logs --boot 2>&1 | Out-String
if ($bootOut -match [regex]::Escape($marker)) {
    Say "PASS: unarr.exe daemon logs --boot returns what is in unarr.boot.log"
} else {
    Say "FAIL: --boot did not return the marker written to $bootLog"
    Say $bootOut
    $fail++
}

# --- 6. the shutdown record, and the Fast Startup blind spot ---------------
# The crash report that prompted this: a Windows box shut down at 00:02 mailed a
# crash report for its own shutdown. BootTime() cannot catch that when Fast
# Startup is on, so the verdict leans on the shutdown record instead. Two things
# only real Windows can prove: that the registry value is written at all, and
# that the Go side parses it to the right instant.
$fsOn = $null
try {
    $fsOn = (Get-ItemProperty -Path 'HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager\Power' -Name HiberbootEnabled -ErrorAction Stop).HiberbootEnabled
} catch { }
Say "fast startup (HiberbootEnabled): $fsOn   [1 = hybrid shutdown, the case BootTime cannot see]"

$rawShutdown = $null
try {
    $bytes = (Get-ItemProperty -Path 'HKLM:\SYSTEM\CurrentControlSet\Control\Windows' -Name ShutdownTime -ErrorAction Stop).ShutdownTime
    $rawShutdown = [datetime]::FromFileTimeUtc([BitConverter]::ToInt64($bytes, 0))
    Say "registry ShutdownTime:  $($rawShutdown.ToString('o'))"
} catch {
    Say "SKIP: no ShutdownTime value in the registry (this machine has never shut down cleanly)"
}

# What Windows logged for the same event, as an independent witness.
$evt = $null
try {
    $evt = (Get-WinEvent -FilterHashtable @{LogName='System'; Id=6006} -MaxEvents 1 -ErrorAction Stop).TimeCreated.ToUniversalTime()
    Say "System event 6006:      $($evt.ToString('o'))"
} catch { Say "note: no 6006 event available to cross-check against" }

# And what the shipped Go code makes of it.
$goShutdown = $null
$m2 = [regex]::Match($sysOut, 'lastShutdown=(\S+)')
if ($m2.Success) {
    $goShutdown = [datetime]::Parse($m2.Groups[1].Value).ToUniversalTime()
    Say "sysinfo.LastShutdown(): $($goShutdown.ToString('o'))"
} elseif ($sysOut -match 'no shutdown-record source') {
    # Only a FAIL when the value is actually THERE and Go could not read it.
    # A guest that has never been shut down cleanly has no ShutdownTime at all -
    # PowerShell skips that case a few lines up, and it must not be a failure
    # here either.
    if ($rawShutdown) {
        Say "FAIL: LastShutdown() found no source, but the registry HAS a ShutdownTime - the read did not work"
        $fail++
    } else {
        Say "SKIP: no ShutdownTime on this machine, so nothing for LastShutdown() to read"
    }
} else {
    Say "FAIL: sysinfo_test.exe did not report a shutdown instant"
    $fail++
}

if ($goShutdown -and $rawShutdown) {
    $d2 = [math]::Abs(($goShutdown - $rawShutdown).TotalSeconds)
    if ($d2 -gt 2) {
        Say "FAIL: LastShutdown() is ${d2}s from the raw registry FILETIME - the parse is wrong"
        $fail++
    } else {
        Say "PASS: LastShutdown() parses the registry FILETIME exactly"
    }
}
if ($goShutdown -and $evt) {
    $d3 = [math]::Abs(($goShutdown - $evt).TotalSeconds)
    # Minutes of slack: the log entry and the registry stamp are written at
    # different points of the same shutdown sequence.
    if ($d3 -gt 300) {
        Say "FAIL: LastShutdown() is ${d3}s from the shutdown Windows logged"
        $fail++
    } else {
        Say "PASS: LastShutdown() agrees with the shutdown Windows logged (delta ${d3}s)"
    }
}

# The blind spot itself, reported rather than asserted: it only shows up on a
# machine that has actually hybrid-shutdown since its last cold boot.
if ($goShutdown -and $osBoot) {
    if ($osBoot -lt $goShutdown) {
        Say "OBSERVED: the boot instant PREDATES the last shutdown - Fast Startup carried the"
        Say "          uptime over a power cycle, so BootTime alone would call that shutdown a crash."
        Say "          This is the exact false-crash-report case the shutdown record now catches."
    } else {
        Say "note: boot is after the last shutdown on this box (cold boot, or Fast Startup off),"
        Say "      so the boot-time verdict would have sufficed here."
    }
}

Say ""
if ($fail -eq 0) { Say "ALL CHECKS PASSED" } else { Say "$fail CHECK(S) FAILED" }

# UTF8 without BOM, so the host reads it back without an encoding dance.
[System.IO.File]::WriteAllLines($out, $lines, (New-Object System.Text.UTF8Encoding($false)))
exit $fail
