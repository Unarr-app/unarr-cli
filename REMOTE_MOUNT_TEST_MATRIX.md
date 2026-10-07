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

## October 7 native feature acceptance harness

The September setup evidence above is preserved. The new opt-in acceptance
harness exercises the production fake-account → catalog → DAV → ranged CDN
path, using six deterministic 2 MiB files and exclusively synthetic identities.
Final native results are recorded separately in
`docs/reviews/remote-mount-native-validation-2026-10-07.md`; preparing or compiling
these tests does not establish a native PASS.

- `TestMountNativeKernelIO`: exact names/sizes, spaces/XML/apostrophe/percent/hash/
  accents/CJK, seek/tail/EOF, eight independent handles, six mutation denials,
  unchanged file bytes, bounded teardown and remount of the same destination.
- `TestMountNativeKernelCancelBlockedRead`: actual mounted read stalled at a
  synthetic CDN, cancellation propagated upstream, listener/process cleanup and
  a fresh readable mount at the same destination.
- `TestMountNativeDAVRecovery` and actual CLI `mount serve --config` subprocesses:
  expired signed URL renewal, short CDN loss/recovery, cancellation, fake free/
  trial/expired/revoked rejection, normal 30s entitlement watcher, and a fresh
  allowed identity. These client response fixtures do not prove website billing
  policy or provider/WAN behavior.
- Native Windows config Save/Load/Validate preserves a bare drive destination;
  optional persistent activation uses the disposable user's actual default
  config/task, then terminal exit, daemon restart and umount. The runner guards,
  renames and restores earlier account directories intact, with identity/ACL
  verification; an existing task/process or unsafe rollback is a failure.
- Linux's actual persistent CLI fixture uses a private Ubuntu/systemd user
  manager and cgroup2 namespace, with the host unit preserved. Both Linux and
  Windows persistent cases save a replacement synthetic key, expire old URLs,
  require fresh accepted resolution and exact mounted bytes within 25s, and
  retain the same daemon PID. Windows additionally requires a limited token and
  unchanged firewall-rule fingerprints. Ambient auth/config overrides are
  rejected by name before CLI subprocesses execute.
- Both persistent fixtures cover disabled config intent with live mount
  resources, then actual umount, and a second ordinary enabled-intent cycle.
  Mount/DAV/rclone must disappear and the ordinary service must become healthy;
  explicit umount may restart it. The unchanged-PID invariant applies to renewal.
- Windows and Mac runners execute existing loopback NNTP cancellation/socket
  recovery, bounded BODY/reply receive and valid complete yEnc decoding tests
  in native package executables from the same candidate SHA.
- Mac controller fixtures and foreground CLI/DAV are independent of kernel
  mounting. Kernel and persistent mounted acceptance remain SKIP while genuine
  macFUSE approval is pending. No installer, policy change or reboot is implied.

Linux example (use the exact-SHA CLI artifact and prepared official rclone):

```sh
GOMAXPROCS=2 GOFLAGS=-p=2 UNARR_NATIVE_ACCEPTANCE=1 UNARR_NATIVE_KERNEL=1 \
  UNARR_NATIVE_CLI=/absolute/task-artifacts/unarr \
  UNARR_NATIVE_RCLONE=/absolute/task-artifacts/rclone \
  go test -v -count=1 -timeout=5m ./internal/cmd -run '^TestMountNative'
```

Windows uses `test/windows/remote-mount-native.ps1` with fresh task/SHA artifact
paths; deploy the script with UTF-8 BOM and CRLF, execute binaries from its local
guest directory, quote Go flags and decode UTF-16 result files. The existing VM,
disk volume/share and older staged binaries/results are preserved. Mac uses
`test/macos/remote-mount-native.sh`; its adjacent document specifies native build,
driver approval and controller-versus-mount evidence boundaries.
Private Linux persistence uses `test/linux/remote-mount-native.sh` and its adjacent
setup/isolation document; it does not operate the host's existing service. All
tests are disabled by default. `TestNativePrepareRclone` authorizes only the existing
official checksum verifier and cannot install or activate a driver.
