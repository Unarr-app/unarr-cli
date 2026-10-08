# Explicitly run in the disposable Windows VM. No provider accounts are used.
$ErrorActionPreference = 'Continue'
$root = '\\host.lan\Data'
$env:UNARR_TEST_DEPENDENCY_DOWNLOAD = '1'
$env:UNARR_TEST_DRIVER_STATE = '1'
& "$root\mountsetup-tests.exe" '-test.v' '-test.timeout=8m' 2>&1 | Out-File "$root\mount-matrix.txt" -Encoding utf8
$result = $LASTEXITCODE
$env:UNARR_TEST_RCLONE_MOUNT = '1'
$env:UNARR_TEST_DEPENDENCY_INSTALL = '1'
& "$root\mount-tests.exe" '-test.v' '-test.timeout=8m' '-test.run=TestRcloneKernelMount|TestMountSetup|TestRemoteLibrary|TestPaidMount|TestMountAccess' 2>&1 | Out-File "$root\mount-matrix.txt" -Append -Encoding utf8
if ($LASTEXITCODE -ne 0) { $result = $LASTEXITCODE }
"EXIT_CODE=$result" | Out-File "$root\mount-matrix.txt" -Append -Encoding utf8
exit $result
