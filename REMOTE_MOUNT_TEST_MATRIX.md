# Remote mount setup validation

Validation date: 2026-09-20. This is a bounded compatibility matrix, not a claim
that every OS release, driver version, architecture and permission combination
has been tested. Tests use synthetic media and no real provider accounts.

## Native execution

| Scenario | Linux amd64 | Windows 11 amd64 VM | macOS 26.6.2 arm64 Mac mini |
| --- | --- | --- | --- |
| Setup decision/failure suite with native subprocess fixtures | Passed | Passed | Passed |
| Official rclone 1.75.1 download, verification and cached reuse | Passed | Passed | Passed |
| Official rclone 1.68.2 compatibility and reuse | Passed | Passed | Passed |
| Official rclone 1.53.4 rejection and private replacement | Passed | Passed | No native arm64 artifact; explicitly skipped |
| Preserve the user's older rclone binary | Passed | Passed | Passed for 1.68.2 |
| Real read-only mount, offset reads and clean unmount | Passed | Passed | Blocked by macOS extension approval |
| Existing driver reused | Passed | Passed (WinFsp 1.12.22301 and 2.1.25156, mounted I/O) | Files reused; inactive extension correctly rejected |
| System driver absent: actual installation | Ubuntu 24.04, Alpine 3.20, Fedora 43, openSUSE Leap 16.0 and Arch base passed | Passed in preceding clean-driver VM test | Passed after official macFUSE uninstall |
| Old driver upgraded | Not measured | 1.12 removal, deferred reboot and 2.1 installation passed | macFUSE 5.1.3 to 5.4.0 passed |
| Native non-elevated UAC accept/cancel | N/A | Both passed with UAC enabled and secure desktop | N/A |
| Security approval and required reboot | N/A | UAC passed; legacy removal returned deferred restart (3010), controller survived | Approval still pending; no reboot performed |

macOS's system log explicitly reports the macFUSE extension as **not approved
to load**. A successful package installation does not mean the driver can mount.
The CLI now reports this state and requests explained consent before trying to
activate the installed extension. It does not reinstall it or silently change
security settings. The Mac is left with macFUSE 5.4.0 installed.

## Deterministic fault coverage

The following run in the native Go test executable on all three OS families.
They inject faults or use native subprocess fixtures; they do not pretend to be
actual historic driver installations or native security dialogs.

- Missing rclone, compatible PATH installation, compatible older installation,
  incompatible older options, mount-less build, failing executable, corrupt cache.
- A second setup reuses the chosen binary; four simultaneous setups download once.
- Hung capability probe, cancellation before setup and while waiting for its lock.
- Refused/unattended driver installation, installer failure, pending restart,
  permission errors and cancellation. No install occurs without prior consent.
- HTTP failure, truncated body, cancellation during transfer, checksum mismatch;
  failed downloads leave no artifacts. Archive traversal, symlink and duplicate
  member rejection are covered by the existing extraction tests.
- Windows-only: absent/truncated/wrong-architecture/non-DLL driver file rejected;
  the installed real WinFsp DLL is also validated in the VM.
- Unix: residual macFUSE bundle and non-executable/missing helpers rejected.

Native Linux isolation checks additionally verified a container without
`/dev/fuse`, and a non-root container with neither sudo nor doas. Both report the
host constraint and stop instead of attempting an impossible installation.

Ubuntu's missing package index and Arch's missing sync database exposed actual
installation failures. Ubuntu now refreshes package indexes. Arch uses a full
`pacman -Syu` transaction (partial upgrades are unsupported), with an explicit
advance warning that ALL installed system packages may be upgraded before consent.
Fedora, openSUSE and Alpine passed their native package-manager paths. Legacy yum
is covered by command-selection tests only.

WinFsp 1.x cannot be upgraded in place by its 2.x installer. The runtime test
exposed error 1603, then verified automatic selection of the legacy MSI product
code and removal. The legacy uninstaller also exposed Restart Manager closing
the controller; setup now disables that behavior and returns the deferred-reboot
result without killing the controller. System installers are staged locally,
because an elevated MSI service could not access the fixture's UNC location.
See the [official WinFsp release notes](https://github.com/winfsp/winfsp/releases/tag/v2.0B2)
for the legacy transition requirement.

## Reproduction

```sh
go test -race ./internal/mountsetup ./internal/cmd ./internal/winproc
UNARR_TEST_DEPENDENCY_DOWNLOAD=1 go test -v -count=1 ./internal/mountsetup
UNARR_TEST_RCLONE_MOUNT=1 go test -v -count=1 ./internal/cmd -run TestRcloneKernelMount
```

`UNARR_TEST_DEPENDENCY_INSTALL=1` additionally authorizes dependency preparation
inside the kernel-mount test. `UNARR_TEST_DRIVER_INSTALL=1` explicitly authorizes
the separate `TestOfficialDriverInstallation` to install/reinstall a driver on
a test machine. Neither is set by ordinary tests or CI. System permission
dialogs still apply; do not use these opt-ins on an unapproved machine.

Windows: build the `internal/mountsetup` and `internal/cmd` test executables as
`test/windows/shared/mountsetup-tests.exe` and `mount-tests.exe`, then run
`test/windows/mount-matrix.ps1` in the disposable VM. It preserves nonzero exit
status and writes `mount-matrix.txt` in the shared directory.

CI now runs the official dependency compatibility suite on its Linux, Windows
and macOS runners. The workflow change has not been remotely executed in this
validation; the native results above come from local Linux, the Windows VM and
the actual Mac mini.

## Remaining release limitations

- Complete macOS security approval and rerun the actual mount before declaring
  macOS end-to-end support validated.
- Old macFUSE package upgrade is tested, old-driver mounted I/O is not (the
  original driver was blocked by macOS). Other old-driver versions are not
  covered by the specific WinFsp 1.12 / macFUSE 5.1.3 cases above.
- No runtime coverage yet for Windows arm64 or Linux arm64, macOS Intel, every
  supported OS release, all historical driver versions, or managed-device policy.
- WAN/provider end-to-end behavior and Zurg comparisons remain outside this
  setup matrix; see REMOTE_MOUNT_PERFORMANCE.md for measured data-path results.
