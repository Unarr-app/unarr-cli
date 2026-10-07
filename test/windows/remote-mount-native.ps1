param(
    [Parameter(Mandatory=$true)][string]$ArtifactDir,
    [Parameter(Mandatory=$true)][ValidatePattern('^[a-f0-9]{40}$')][string]$SourceSHA,
    [Parameter(Mandatory=$true)][ValidatePattern('^[a-zA-Z0-9_-]+$')][string]$TaskTag,
    [switch]$Kernel,
    [switch]$Persistent
)

# Deploy this script as UTF-8 BOM + CRLF for Windows PowerShell 5.1.
# Existing VM, installed WinFsp, shared volume and older results are preserved.
$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)
$OutputEncoding = [Console]::OutputEncoding
$local = Join-Path 'C:\unarrtest\remote-mount' ($TaskTag + '-' + $SourceSHA)
$result = Join-Path $ArtifactDir 'windows-native-result.txt'
if (Test-Path $local) { throw 'Fresh task/SHA directory required; refusing stale binaries.' }
if (Test-Path $result) { throw 'Fresh result required; refusing to overwrite prior evidence.' }
if ($Persistent -and -not $Kernel) { throw 'Persistent drive acceptance requires -Kernel.' }
if ($Kernel -and -not (Test-Path 'C:\Program Files (x86)\WinFsp\bin\winfsp-x64.dll')) {
    throw 'WinFsp must already be installed; this harness never installs a driver.'
}
New-Item -ItemType Directory -Path $local | Out-Null
foreach ($name in @('unarr.exe', 'cmd-native.test.exe', 'mountsetup-native.test.exe', 'nntp-native.test.exe', 'yenc-native.test.exe')) {
    Copy-Item -LiteralPath (Join-Path $ArtifactDir $name) -Destination (Join-Path $local $name)
}
Set-Location -LiteralPath $local
$clock = [System.Diagnostics.Stopwatch]::StartNew()
$lines = New-Object 'System.Collections.Generic.List[string]'
$lines.Add('SOURCE_SHA=' + $SourceSHA)
$lines.Add('TASK=' + $TaskTag)
$lines.Add('NATIVE_OS=' + [Environment]::OSVersion.VersionString)
$lines.Add('POWERSHELL=' + $PSVersionTable.PSVersion.ToString())
$lines.Add('NATIVE_EXECUTION=windows/amd64; cross-compilation alone is not acceptance')
foreach ($name in @('unarr.exe', 'cmd-native.test.exe', 'mountsetup-native.test.exe', 'nntp-native.test.exe', 'yenc-native.test.exe')) {
    $lines.Add('SHA256 ' + $name + '=' + (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $local $name)).Hash)
}
$env:UNARR_NATIVE_CLI = Join-Path $local 'unarr.exe'
$env:UNARR_NATIVE_TOOLS_DIR = Join-Path $local 'tools'
$env:UNARR_NATIVE_PREPARE_RCLONE = '1'
$env:UNARR_NATIVE_ACCEPTANCE = '1'
$env:UNARR_NATIVE_KERNEL = '0'
$env:UNARR_NATIVE_WINDOWS_SERVICE = '0'
if ($Kernel) { $env:UNARR_NATIVE_KERNEL = '1' }
if ($Persistent) { $env:UNARR_NATIVE_WINDOWS_SERVICE = '1' }
$env:UNARR_NO_TELEMETRY = '1'
# Persistent commands require ordinary service environment; telemetry is disabled
# in the synthetic config and UNARR_NO_TELEMETRY gates crash reporting.
# An inherited telemetry override is preserved and rejected by the persistent
# guard, because the scheduled task would inherit the account environment too.
$env:GOMAXPROCS = '2'
$exitCode = 1
$originals = @()
$fixturesPrepared = $false
$firewallBefore = ''
$drivesBefore = @([System.IO.Directory]::GetLogicalDrives())
function Get-DirectoryIdentity([string]$path) {
    $item = Get-Item -LiteralPath $path -ErrorAction Stop
    $acl = (Get-Acl -LiteralPath $path -ErrorAction Stop).Sddl
    $fileId = (& fsutil file queryfileid $path 2>&1 | Out-String).Trim()
    if ($LASTEXITCODE -ne 0) { throw 'Cannot verify original directory file ID.' }
    return ($item.CreationTimeUtc.Ticks.ToString() + '|' + $acl + '|' + $fileId)
}
function Get-UnarrFirewallFingerprint {
    $parts = @()
    foreach ($name in @('unarr (BitTorrent peers)','unarr (peer TCP)','unarr (peer UDP / DHT)')) {
        $text = (& netsh advfirewall firewall show rule ('name=' + $name) 2>&1 | Out-String)
        if ($LASTEXITCODE -ne 0 -and $text -notmatch 'No rules match') { throw 'Cannot verify read-only firewall snapshot.' }
        $parts += $name
        $parts += ($text -split "`n" | ForEach-Object {$_.Trim()} | Sort-Object)
    }
    $hash = [Security.Cryptography.SHA256]::Create()
    try { return ([BitConverter]::ToString($hash.ComputeHash([Text.Encoding]::UTF8.GetBytes(($parts -join "`n"))))).Replace('-','') }
    finally { $hash.Dispose() }
}
try {
    if ($Persistent) {
        $principal = [Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
        if ($principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { throw 'Limited token required; no firewall policy changes or elevation permitted.' }
        $lines.Add('TOKEN_ELEVATED=0; scheduled task LeastPrivilege')
        foreach ($name in @('UNARR_CONFIG_DIR','UNARR_API_KEY','UNARR_API_URL','UNARR_DOWNLOAD_DIR','UNARR_COUNTRY','UNARR_TELEMETRY')) {
            if ([Environment]::GetEnvironmentVariable($name)) { throw ('Normal service environment required: ' + $name) }
        }
        $ErrorActionPreference = 'Continue'
        $firewallBefore = Get-UnarrFirewallFingerprint
        $lines.Add('FIREWALL_BEFORE_SHA256=' + $firewallBefore)
        $task = & schtasks /Query /TN unarr 2>&1 | Out-String
        if ($LASTEXITCODE -eq 0 -or $task -notmatch 'cannot find') { throw 'Cannot confirm unarr scheduled task absence.' }
        $existing = @(Get-CimInstance Win32_Process | Where-Object {$_.Name -like 'unarr*' -or $_.Name -eq 'rclone.exe'})
        if ($existing.Count -ne 0) { throw 'Existing native processes prevent disposable account fixture.' }
        $ErrorActionPreference = 'Stop'
        foreach ($path in @((Join-Path $env:APPDATA 'unarr'), (Join-Path $env:LOCALAPPDATA 'unarr'))) {
            $backup = $path + '.native-preserved-' + $TaskTag + '-' + $SourceSHA
            if (Test-Path -LiteralPath $backup) { throw 'Unique original-directory backup already exists.' }
            $record = [pscustomobject]@{Path=$path; Backup=$backup; Moved=$false; Identity=''}
            if (Test-Path -LiteralPath $path) { $record.Identity = Get-DirectoryIdentity $path }
            $originals += $record
        }
        # Both backup paths were verified before the first same-volume rename.
        foreach ($record in $originals) {
            if ($record.Identity) {
                Move-Item -LiteralPath $record.Path -Destination $record.Backup -ErrorAction Stop
                $record.Moved = $true
                $lines.Add('PRESERVED_DIRECTORY=' + $record.Path + '|' + $record.Identity)
            }
        }
        $fixturesPrepared = $true
    }
    # Temporarily allow native stderr records; expected cancelled-read diagnostics
    # do not abort PowerShell before the Go test exit code is recorded.
    $ErrorActionPreference = 'Continue'
    $setupOutput = & (Join-Path $local 'mountsetup-native.test.exe') '-test.v' '-test.run' '^TestNativePrepareRclone$' '-test.timeout' '6m' 2>&1
    $setupExit = $LASTEXITCODE
    foreach ($line in $setupOutput) { $lines.Add($line.ToString()) }
    $lines.Add('SETUP_EXIT=' + $setupExit)
    if ($setupExit -ne 0) { throw 'Verified rclone preparation failed.' }
    $rclone = Get-ChildItem -LiteralPath $env:UNARR_NATIVE_TOOLS_DIR -Filter 'rclone.exe' -Recurse | Select-Object -First 1
    if ($null -eq $rclone) { throw 'No task-local verified rclone binary found.' }
    $env:UNARR_NATIVE_RCLONE = $rclone.FullName
    foreach ($line in (& $rclone.FullName version 2>&1)) { $lines.Add($line.ToString()) }
    $output = & (Join-Path $local 'cmd-native.test.exe') '-test.v' '-test.run' '^TestMountNative' '-test.timeout' '5m' 2>&1
    $exitCode = $LASTEXITCODE
    foreach ($line in $output) { $lines.Add($line.ToString()) }
    $lines.Add('TOP_LEVEL_PASS=' + @($output | Where-Object {$_.ToString() -match '^--- PASS:'}).Count)
    $lines.Add('TOP_LEVEL_FAIL=' + @($output | Where-Object {$_.ToString() -match '^--- FAIL:'}).Count)
    $lines.Add('TOP_LEVEL_SKIP=' + @($output | Where-Object {$_.ToString() -match '^--- SKIP:'}).Count)
    $protocol = @(
        @('nntp', '^Test(Boundary(CancelInFlight|CancelDuringRepairHandshake|EncodedBodyCeiling|SmallReceiveLimits|OversizeRetiresAndRecovers|IncompleteWireRetiresAndRetries)|Final(OversizedReplyPublicAPI|ReplyCeilingPublicAPI|ReplyContinuationsCannotBeCleanAlternatives|ReplyFramingAndBufferedBytes)|TimedOutConnectionIsNeverReused|MidBodyResetsNeverLeakPoolSlots)$'),
        @('yenc', '^Test(Decode(SimpleArticle|Multipart|BinaryData)|EncodeDecodeRoundTrip(SinglePart|Multipart)|BoundaryYEncIntegrity|Final(HeaderFieldsExcludeFilename|ExactMultipartHeaderFields))$')
    )
    foreach ($case in $protocol) {
        $lines.Add('PROTOCOL_COMMAND=' + $case[0] + '-native.test.exe -test.v -test.run ' + $case[1] + ' -test.timeout 90s')
        $output = & (Join-Path $local ($case[0] + '-native.test.exe')) '-test.v' '-test.run' $case[1] '-test.timeout' '90s' 2>&1
        $protocolExit = $LASTEXITCODE
        foreach ($line in $output) { $lines.Add($line.ToString()) }
        $lines.Add('PROTOCOL_' + $case[0] + '_EXIT=' + $protocolExit)
        $lines.Add('PROTOCOL_' + $case[0] + '_PASS=' + @($output | Where-Object {$_.ToString() -match '^--- PASS:'}).Count)
        $lines.Add('PROTOCOL_' + $case[0] + '_FAIL=' + @($output | Where-Object {$_.ToString() -match '^--- FAIL:'}).Count)
        if ($protocolExit -ne 0) { $exitCode = 1 }
    }
} catch {
    $lines.Add('HARNESS_ERROR=' + $_.Exception.Message)
    $exitCode = 1
} finally {
    if ($Persistent -and $originals.Count -gt 0) {
        try {
            $ErrorActionPreference = 'Continue'
            & schtasks /Query /TN unarr *> $null
            if ($fixturesPrepared -and $LASTEXITCODE -eq 0) {
                $cleanup = Start-Process -FilePath $env:UNARR_NATIVE_CLI -ArgumentList @('daemon','uninstall') -PassThru -WindowStyle Hidden
                if (-not $cleanup.WaitForExit(35000)) { $cleanup.Kill(); $lines.Add('CLEANUP_UNINSTALL_TIMEOUT=1'); $exitCode = 1 }
            }
            # Only processes whose executable is in this fixture can be killed.
            $freshTools = (Join-Path $env:APPDATA 'unarr\tools\')
            foreach ($process in @(Get-CimInstance Win32_Process | Where-Object {$_.Name -like 'unarr*' -or $_.Name -eq 'rclone.exe'})) {
                if (-not $fixturesPrepared) { throw 'Process appeared before fixture activation; backups remain intact.' }
                $owned = $process.ExecutablePath -eq $env:UNARR_NATIVE_CLI
                $owned = $owned -or ($process.Name -eq 'rclone.exe' -and $process.ExecutablePath -and $process.ExecutablePath.StartsWith($freshTools, [StringComparison]::OrdinalIgnoreCase))
                if (-not $owned) { throw 'Unknown process prevents safe rollback; preserved backups remain intact.' }
                Stop-Process -Id $process.ProcessId -Force -ErrorAction Stop
            }
            & schtasks /Query /TN unarr *> $null
            if ($LASTEXITCODE -eq 0) { throw 'Scheduled task survived cleanup; preserved backups remain intact.' }
            $release = [System.Diagnostics.Stopwatch]::StartNew()
            do {
                $newDrives = @([System.IO.Directory]::GetLogicalDrives() | Where-Object {$drivesBefore -notcontains $_})
                if ($newDrives.Count -eq 0) { break }
                Start-Sleep -Milliseconds 50
            } while ($release.ElapsedMilliseconds -lt 8000)
            if ($newDrives.Count -ne 0) { throw 'Fixture mounted drive survived cleanup; backups remain intact.' }
            $ErrorActionPreference = 'Stop'
            foreach ($record in $originals) {
                # Never remove an original whose rename did not complete.
                if (($record.Moved -or ($fixturesPrepared -and -not $record.Identity)) -and (Test-Path -LiteralPath $record.Path)) {
                    Remove-Item -LiteralPath $record.Path -Recurse -Force -ErrorAction Stop
                }
                if ($record.Moved) {
                    Move-Item -LiteralPath $record.Backup -Destination $record.Path -ErrorAction Stop
                    if ((Get-DirectoryIdentity $record.Path) -ne $record.Identity) { throw 'Original directory identity/ACL changed during restoration.' }
                    $lines.Add('RESTORED_DIRECTORY=' + $record.Path + '|IDENTITY_MATCH=1')
                }
            }
            $left = @(Get-CimInstance Win32_Process | Where-Object {$_.Name -like 'unarr*' -or $_.Name -eq 'rclone.exe'})
            if ($left.Count -ne 0) { throw 'Native fixture process survived restoration.' }
            $lines.Add('PERSISTENT_CLEANUP=PASS; no task/process/mount; originals restored')
            $ErrorActionPreference = 'Continue'
            $firewallAfter = Get-UnarrFirewallFingerprint
            $lines.Add('FIREWALL_AFTER_SHA256=' + $firewallAfter)
            if ($firewallBefore -and $firewallAfter -ne $firewallBefore) { throw 'Firewall policy changed; original directories restored, escalate without changing policy.' }
        } catch {
            $lines.Add('ROLLBACK_BLOCKED=' + $_.Exception.Message)
            $exitCode = 1
        }
    }
    $clock.Stop()
    $lines.Add('DURATION_MS=' + $clock.ElapsedMilliseconds)
    $lines.Add('EXIT=' + $exitCode)
    $lines | Out-File -LiteralPath $result -Encoding Unicode
    foreach ($name in @('UNARR_NATIVE_CLI','UNARR_NATIVE_TOOLS_DIR','UNARR_NATIVE_PREPARE_RCLONE','UNARR_NATIVE_ACCEPTANCE','UNARR_NATIVE_KERNEL','UNARR_NATIVE_WINDOWS_SERVICE','UNARR_NATIVE_RCLONE')) {
        Remove-Item ('Env:' + $name) -ErrorAction SilentlyContinue
    }
}
exit $exitCode
