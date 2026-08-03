# Daemon log ROTATION and OWNERSHIP checks - REAL Windows only.
#
# WHAT THIS REPLACES. An earlier version of this file MEASURED the old design
# and found it broken: with the shim redirecting the daemon into unarr.log with
# ">>", cmd.exe granted only FILE_SHARE_READ, so "unarr logs rotate" failed with
#   truncate log file: ... The process cannot access the file because it is
#   being used by another process
# while the snapshot copy had ALREADY been made. Per 60s janitor tick that
# copied a whole budget aside (about 28 GB/day at the 20 MB default) and the
# live log never shrank. Silently.
#
# WHAT THIS ASSERTS NOW. The daemon owns its own log: it opens unarr.log itself
# with O_APPEND (a real FILE_APPEND_DATA handle) and rotates it by RENAME, which
# works because the renamer holds the descriptor. cmd.exe keeps only the small
# unarr.boot.log. So:
#   [1] the shim's ">>" target and its --log-file target are DIFFERENT files
#       (were they the same, cmd's FILE_SHARE_READ would refuse the daemon's
#       FILE_APPEND_DATA open and the daemon would have NO log at all)
#   [2] unarr.log really SHRINKS while the daemon runs, under a live cmd.exe
#       redirect holder - the exact assertion that failed before
#   [3] output that bypasses log.SetOutput (the banner, a fatal start error)
#       lands in unarr.boot.log and not in the owned log
#   [4] the shim bounds unarr.boot.log itself, by rename, at its 2 MB budget
#   [5] "unarr logs" and "unarr logs --boot" each read their own file
#   [6] a rename BLOCKED by a live reader ("unarr logs -f", an antivirus,
#       Windows Search) leaves the rotated ring untouched - the rotation fails,
#       and failing must cost nothing
#   [7] an external "unarr logs rotate" stands down while the daemon owns the
#       log, and works again once that owner is gone (a stale state file from a
#       crash must not block rotation forever)
#
# A regression to the old design fails [1] and [2]. A regression to the
# shift-the-ring-first order fails [6]; a regression to probing the filesystem
# instead of reading the daemon's claim fails [7].
#
# Run:  powershell -ExecutionPolicy Bypass -File \\host.lan\Data\smoke-rotation.ps1
# Writes progress to \\host.lan\Data\rotation-result.txt (watch it from the host).
#
# HARNESS RULES (test/windows/README.md): ASCII only, no here-strings, config
# files without a BOM, and never time anything with Get-Date deltas - the guest
# clock resyncs against the host and jumps backwards, which leaves a Get-Date
# deadline unreachable and hangs the run. Use a Stopwatch.

$ErrorActionPreference = 'Continue'
$Shared  = '\\host.lan\Data'
$Out     = "$Shared\rotation-result.txt"
$WorkDir = 'C:\unarr'
$DataDir = "$env:LOCALAPPDATA\unarr"
$CfgDir  = "$env:APPDATA\unarr"
$BadCfg  = "$WorkDir\emptycfg"

$Log   = "$DataDir\unarr.log"
$Log1  = "$DataDir\unarr.log.1"
$Boot  = "$DataDir\unarr.boot.log"
$Boot1 = "$DataDir\unarr.boot.log.1"

# The budgets under test. LogBudget mirrors the config written below;
# BootBudget is FIXED in the binary (bootLogMaxSizeMB) and not configurable —
# but it is GATED on log_max_size_mb being non-zero, and the shim's copy of it
# is baked in at `daemon install` time. That is why the config below is written
# BEFORE the install: rotation is opt-in and off by default, so an install done
# first would produce a shim with no boot-log trim in it at all.
$LogBudget  = 1MB
$BootBudget = 2MB

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
function SizeOf($path) {
    if (Test-Path $path) { return (Get-Item $path).Length }
    return -1
}
# Monotonic wait. See the harness rules above for why this is not Get-Date.
function WaitFor([scriptblock]$cond, [int]$seconds, [string]$what) {
    Say "  waiting up to ${seconds}s for $what ..."
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    while ($sw.Elapsed.TotalSeconds -lt $seconds) {
        if (& $cond) { return $true }
        Start-Sleep -Milliseconds 500
    }
    return $false
}
function KillDaemons {
    Get-Process unarr   -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
    Get-Process wscript -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
    Start-Sleep -Seconds 2
}
# Fill a file with n bytes of printable ASCII, without a BOM.
function SeedFile($path, $bytes) {
    $line = ('A' * 127) + "`r`n"
    $sb = New-Object System.Text.StringBuilder
    while ($sb.Length -lt $bytes) { [void]$sb.Append($line) }
    [System.IO.File]::WriteAllText($path, $sb.ToString(),
        (New-Object System.Text.UTF8Encoding($false)))
}
# Launch the daemon EXACTLY the way the VBS shim does: one cmd.exe, the daemon
# owning unarr.log through --log-file, cmd owning the boot log through ">>".
# The handle semantics under test are cmd.exe's, so the wrapper must be real.
function StartLikeShim {
    $inner = '"' + "$WorkDir\unarr.exe" + '" start --log-file "' + $Log + '" >> "' + $Boot + '" 2>&1'
    return Start-Process -FilePath 'cmd.exe' -ArgumentList '/c', $inner -PassThru -WindowStyle Hidden
}

Remove-Item $Out -ErrorAction SilentlyContinue
Say "=== unarr log ownership + rotation checks on $(([System.Environment]::OSVersion).VersionString) ==="

# --- setup ------------------------------------------------------------------
Say "[setup] staging binaries + config"
New-Item -ItemType Directory -Force -Path $WorkDir | Out-Null
New-Item -ItemType Directory -Force -Path $DataDir | Out-Null
New-Item -ItemType Directory -Force -Path $BadCfg  | Out-Null
Copy-Item "$Shared\unarr.exe" $WorkDir -Force
$hasFakeApi = Test-Path "$Shared\fakeapi.exe"
if ($hasFakeApi) { Copy-Item "$Shared\fakeapi.exe" $WorkDir -Force }

& "$WorkDir\unarr.exe" daemon uninstall 2>&1 | Out-Null
schtasks /delete /tn unarr /f 2>&1 | Out-Null
KillDaemons
Get-Process fakeapi -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
Remove-Item $Log, $Log1, $Boot, $Boot1, "$DataDir\daemon.state.json", "$DataDir\daemon.stopped" -ErrorAction SilentlyContinue

if ($hasFakeApi) {
    # A stub backend so the daemon can complete a real registration offline and
    # STAY UP - check [2] needs a daemon that lives long enough to rotate.
    $env:ADDR = '127.0.0.1:18080'
    Start-Process -FilePath "$WorkDir\fakeapi.exe" -WindowStyle Hidden
    Start-Sleep -Seconds 2
} else {
    Say "  NOTE: fakeapi.exe not found; the daemon will not stay up and [2] will be inconclusive"
    Say "  build it with: cd test/windows and GOOS=windows go build -o shared/fakeapi.exe (lab stub)"
}

# Config as an array, NOT a here-string: PowerShell 5.1 fails to find the
# terminator in an LF file and swallows the rest of the script.
$cfg = @(
    '[auth]',
    'api_key = "win-lab-key"',
    'api_url = "http://127.0.0.1:18080"',
    'mirrors = []',
    '',
    '[agent]',
    'id = "win-lab-agent"',
    'name = "win-lab"',
    '',
    '[downloads]',
    'dir = "C:\\unarr\\downloads"',
    'stream_port = 21818',
    'https_stream_port = 21819',
    'auto_https_upnp = false',
    'enable_upnp = false',
    '',
    '[telemetry]',
    'enabled = false',
    '',
    '[library]',
    'auto_scan = false',
    '',
    '[daemon]',
    'log_max_size_mb = 1',
    'log_max_files = 3'
)
New-Item -ItemType Directory -Force -Path $CfgDir | Out-Null
New-Item -ItemType Directory -Force -Path "$WorkDir\downloads" | Out-Null
# WITHOUT a BOM: Set-Content -Encoding UTF8 on 5.1 adds one, and three stray
# bytes in front of the first TOML key make the config parse as EMPTY - the
# daemon then exits with "no API key configured", which reads exactly like a
# startup crash.
[System.IO.File]::WriteAllText("$CfgDir\config.toml",
    (($cfg -join "`r`n") + "`r`n"), (New-Object System.Text.UTF8Encoding($false)))
$firstBytes = [System.IO.File]::ReadAllBytes("$CfgDir\config.toml")[0..2]
Check ($firstBytes[0] -ne 0xEF) "config.toml written without a BOM"

# --- 1. the two files must be two files -------------------------------------
Say "[1] the shim redirects somewhere OTHER than the log it hands the daemon"
& "$WorkDir\unarr.exe" daemon install 2>&1 | Out-Null
Start-Sleep -Seconds 5

$vbsPath = "$DataDir\unarr-launch.vbs"
Check (Test-Path $vbsPath) "launcher shim written to $vbsPath"
if (Test-Path $vbsPath) {
    $bytes = [System.IO.File]::ReadAllBytes($vbsPath)
    Check ($bytes[0] -eq 0xFF -and $bytes[1] -eq 0xFE) "shim is UTF-16LE with a BOM (WSH decodes it correctly)"
    # WSH reads the shim as UTF-16, so read it back the same way.
    $vbs = [System.IO.File]::ReadAllText($vbsPath, [System.Text.Encoding]::Unicode)

    Check ($vbs -match 'start --log-file') "shim hands the daemon its OWN log with --log-file"
    Check ($vbs -match 'unarr\.boot\.log') "shim redirects into unarr.boot.log"
    # THE invariant. Redirecting into the same file the daemon opens
    # FILE_APPEND_DATA would leave the daemon with no log at all.
    Check (-not ($vbs -match '>> ""[^"]*unarr\.log""')) "shim does NOT redirect into the log the daemon owns"
    Check ($vbs -match 'fso\.MoveFile') "shim bounds the boot log itself (nothing else can)"
    # Supervision must survive this change: RestartOnFailure does NOT act on a
    # non-zero action exit code (measured), so the relaunch loop IS the supervisor.
    Check ($vbs -match 'WScript\.Quit 1') "shim still reports failure to Task Scheduler"
    Check ($vbs -match 'Loop') "shim still supervises in a relaunch loop"

    # Keep a copy: daemon uninstall deletes the original, and check [4] needs it.
    Copy-Item $vbsPath "$WorkDir\shim-copy.vbs" -Force
}

# The install ran the task, which started a daemon. Clear the field before the
# rotation check so exactly ONE daemon owns the log.
schtasks /end /tn unarr 2>&1 | Out-Null
& "$WorkDir\unarr.exe" daemon uninstall 2>&1 | Out-Null
KillDaemons
Remove-Item $Log, $Log1, $Boot, $Boot1, "$DataDir\daemon.state.json", "$DataDir\daemon.stopped" -ErrorAction SilentlyContinue

# --- 2. the live log must actually shrink -----------------------------------
Say "[2] unarr.log SHRINKS while the daemon runs, under a live cmd.exe holder"
SeedFile $Log (2 * $LogBudget)
$seeded = SizeOf $Log
Say "  seeded unarr.log = $seeded bytes (budget $LogBudget)"

$holder = StartLikeShim
Say "  daemon launched through cmd.exe, PID $($holder.Id)"

# The Writer seeds its counter from Stat and rotates on the FIRST write, so the
# rename lands within a second or two of the run marker.
$shrank = WaitFor { (SizeOf $Log) -ge 0 -and (SizeOf $Log) -lt $LogBudget } 60 "the rename rotation"
Say "  live unarr.log after rotation = $(SizeOf $Log) bytes"
Say "  unarr.log.1 = $(SizeOf $Log1) bytes"

Check $shrank "the live log SHRANK (the old copy-truncate design left it at its seeded size)"
Check ((SizeOf $Log1) -ge $seeded) "the rotated slot holds the previous contents"

# The daemon must still be logging INTO the fresh file after the rename: a
# rotation that leaves the daemon writing to a detached inode is silent death.
$reopened = WaitFor { (SizeOf $Log) -gt 0 } 60 "the daemon to write into the fresh log"
Check $reopened "the daemon keeps logging after the rotation"
if ($reopened) {
    $head = Get-Content $Log -TotalCount 5 -ErrorAction SilentlyContinue | Out-String
    Check ($head -match 'starting \(pid') "the fresh log opens with the run marker (the only run delimiter left)"
}

# --- 3. what bypasses the writer must land in the boot log ------------------
Say "[3] the boot log collects what never reaches the daemon's own log"
$bootHasBanner = WaitFor { (Test-Path $Boot) -and ((Get-Content $Boot -Raw -ErrorAction SilentlyContinue) -match 'unarr Daemon') } 60 "the banner in the boot log"
Check $bootHasBanner "the start banner is in unarr.boot.log (fmt.Println bypasses log.SetOutput)"

KillDaemons
Remove-Item "$DataDir\daemon.state.json", "$DataDir\daemon.stopped" -ErrorAction SilentlyContinue

# A start that dies BEFORE the Writer exists: cobra prints the fatal error to
# stderr, which is the boot log. This is the whole reason the boot log exists.
$logBefore = SizeOf $Log
$savedCfgDir = $env:UNARR_CONFIG_DIR
$env:UNARR_CONFIG_DIR = $BadCfg
$dead = StartLikeShim
Start-Sleep -Seconds 8
$env:UNARR_CONFIG_DIR = $savedCfgDir
if (-not $dead.HasExited) { Stop-Process -Id $dead.Id -Force -ErrorAction SilentlyContinue }

$bootText = Get-Content $Boot -Raw -ErrorAction SilentlyContinue
Check ($bootText -match 'no API key configured') "a fatal start error is captured in unarr.boot.log"
Check ((SizeOf $Log) -eq $logBefore) "that failure did NOT touch the owned log (it died before the Writer existed)"

# --- 4. the shim bounds the boot log itself ---------------------------------
Say "[4] the shim renames the boot log aside at its fixed $BootBudget-byte budget"
KillDaemons
Remove-Item $Boot1 -ErrorAction SilentlyContinue
SeedFile $Boot (2 * $BootBudget)
Say "  seeded unarr.boot.log = $(SizeOf $Boot) bytes (fixed budget $BootBudget)"

# The trim runs at the TOP of the shim's loop, before sh.Run - the one moment
# nothing holds the file. So it fires within seconds of wscript starting, long
# before the daemon it launches matters.
$wsh = Start-Process -FilePath 'wscript.exe' -ArgumentList '//B', '//Nologo', "$WorkDir\shim-copy.vbs" -PassThru
$trimmed = WaitFor { (Test-Path $Boot1) -and ((SizeOf $Boot) -lt $BootBudget) } 60 "the shim to trim the boot log"
Check $trimmed "the shim rotated unarr.boot.log by rename (copy-truncate cannot bound a file cmd.exe holds)"
Say "  after trim: boot=$(SizeOf $Boot) boot.1=$(SizeOf $Boot1)"

if (-not $wsh.HasExited) { Stop-Process -Id $wsh.Id -Force -ErrorAction SilentlyContinue }
KillDaemons

# --- 5. each reader reads its own file --------------------------------------
Say "[5] unarr logs and unarr logs --boot read different files"
$mainOut = & "$WorkDir\unarr.exe" logs -n 200 2>&1 | Out-String
Check ($mainOut -match 'starting \(pid') "unarr logs reads the daemon's own log"

$bootOut = & "$WorkDir\unarr.exe" logs --boot -n 200 2>&1 | Out-String
Check ($bootOut -match 'unarr Daemon' -or $bootOut -match 'no API key configured') "unarr logs --boot reads the startup log"
Check (-not ($bootOut -match 'starting \(pid')) "unarr logs --boot does NOT fall back to the daemon's own log"

# --- 6. a BLOCKED rename must not cost the history --------------------------
# The measured failure this section exists for: "unarr logs -f" (the command
# "unarr daemon install" itself prints) keeps the live file open, Go asks for
# FILE_SHARE_READ|FILE_SHARE_WRITE and never FILE_SHARE_DELETE, so MoveFileEx
# cannot rename it. The rotation is SUPPOSED to fail here. What must not happen
# is the old behaviour: shift the ring first, so every failed attempt dropped
# the oldest slot and moved the rest, emptying all three in three budgets while
# the live log grew without a ceiling.
Say "[6] a rename blocked by a live reader leaves the ring INTACT"
KillDaemons
Remove-Item $Log, $Log1, "$DataDir\unarr.log.2", "$DataDir\unarr.log.3", "$DataDir\daemon.state.json" -ErrorAction SilentlyContinue
$enc = New-Object System.Text.UTF8Encoding($false)
[System.IO.File]::WriteAllText($Log1, 'HISTORY-SLOT-1', $enc)
[System.IO.File]::WriteAllText("$DataDir\unarr.log.2", 'HISTORY-SLOT-2', $enc)
[System.IO.File]::WriteAllText("$DataDir\unarr.log.3", 'HISTORY-SLOT-3', $enc)
SeedFile $Log (2 * $LogBudget)

$holder6 = StartLikeShim
$follower = Start-Process -FilePath "$WorkDir\unarr.exe" -ArgumentList 'logs', '-f' -PassThru -WindowStyle Hidden
Say "  daemon PID $($holder6.Id), follower (unarr logs -f) PID $($follower.Id)"
# Give the daemon time to write its run marker, hit the budget and fail the
# rename several times over.
Start-Sleep -Seconds 20

$slot1 = Get-Content $Log1 -Raw -ErrorAction SilentlyContinue
$slot2 = Get-Content "$DataDir\unarr.log.2" -Raw -ErrorAction SilentlyContinue
$slot3 = Get-Content "$DataDir\unarr.log.3" -Raw -ErrorAction SilentlyContinue
Say "  slots after the blocked rotation: [1]=$slot1 [2]=$slot2 [3]=$slot3"
Check ($slot1 -eq 'HISTORY-SLOT-1') "unarr.log.1 still holds its history"
Check ($slot2 -eq 'HISTORY-SLOT-2') "unarr.log.2 still holds its history"
Check ($slot3 -eq 'HISTORY-SLOT-3') "unarr.log.3 still holds its history"
Check (-not (Test-Path "$Log.rotating")) "no staging file was left behind"

if (-not $follower.HasExited) { Stop-Process -Id $follower.Id -Force -ErrorAction SilentlyContinue }

# --- 7. an external rotation stands down under a live owner -----------------
# "unarr logs rotate" cannot copy-truncate a file the daemon owns: the probe it
# used to rely on CANNOT see a Go owner (FILE_SHARE_WRITE, no lock), so the
# answer has to come from the daemon's own claim in daemon.state.json.
Say "[7] unarr logs rotate stands down while the daemon owns the log"
$sizeBefore = SizeOf $Log
$rotOut = & "$WorkDir\unarr.exe" logs rotate 2>&1 | Out-String
Say "  logs rotate said: $($rotOut.Trim())"
Check ($LASTEXITCODE -eq 0) "it exits 0 - a daemon rotating its own log is not an error"
Check ($rotOut -match 'owned by') "it explains WHO owns the file"
Check ((SizeOf $Log) -eq $sizeBefore) "it did not truncate the owner's live file"
Check ((Get-Content $Log1 -Raw -ErrorAction SilentlyContinue) -eq 'HISTORY-SLOT-1') "it did not shift the ring"

# ... and a STALE claim must not block it forever: kill the daemon (leaving its
# state file behind, exactly as a crash does) and the same command must work.
KillDaemons
Check (Test-Path "$DataDir\daemon.state.json") "the killed daemon left its state file behind (the stale-claim case)"
& "$WorkDir\unarr.exe" logs rotate 2>&1 | Out-Null
Check ($LASTEXITCODE -eq 0) "logs rotate succeeds once the owner is gone"
Check ((SizeOf $Log) -lt $LogBudget) "the log was trimmed - a dead PID's claim does not block rotation"

# --- teardown ---------------------------------------------------------------
Say "[teardown]"
& "$WorkDir\unarr.exe" daemon uninstall 2>&1 | Out-Null
KillDaemons
Get-Process fakeapi -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue

Say ""
Say "RESULT passed=$($script:pass) failed=$($script:fail)"
Say "DONE"
if ($script:fail -gt 0) { exit 1 } else { exit 0 }
