# Private native Linux persistence fixture

The runner uses the native Linux kernel, a disposable Ubuntu24.04 filesystem and
an actual systemd255 user manager. It runs the real CLI's persistent `mount`,
waits for readable mounted IO after the initiating process exits, restarts the
daemon, reads again, then `umount`s while the ordinary daemon stays alive. Its
renewal step replaces the saved synthetic key, expires previous CDN URLs and
requires fresh accepted resolution plus exact mounted bytes within 25s, with
the same live daemon PID. One read can be pending at a time; the deadline remains
bounded even if that read stalls, and fixture cleanup tears down its service.
Go test refuses host/root execution, any existing service/config/data directory,
and nonservice environment overrides. This is distinct from foreground DAV tests.

Build CLI and `internal/cmd` test binaries from the same recorded candidate SHA;
use `CGO_ENABLED=0` for a portable CLI artifact, bounded `GOMAXPROCS=4 -p4` only
after the coordinator grants the build slot. Provide official rclone1.75.1
already prepared by `TestNativePrepareRclone`'s checksum verifier. Example:

```sh
UNARR_NATIVE_FIXTURE_IMAGE=unarr-native-systemd:ctx2cb6aa585b2e \
  bash test/linux/remote-mount-native.sh "$SOURCE_SHA" "$TASK_TAG" \
  "$ARTIFACT_DIR/unarr" "$ARTIFACT_DIR/cmd-native.test" \
  /tmp/unarr-native-ctx_2cb6aa585b2e/tools/rclone-1.75.1-linux-amd64/rclone
```

Without the optional prepared image, the runner provisions packages in a unique
temporary container from cached `ubuntu:24.04` (`--pull never`) and removes its
owned preparation container/image afterward. No Go builds occur in the runner.
Native package/tool versions and SHA-256 provenance are written under its printed
fresh `/tmp/unarr-native-...` artifact directory.

Isolation is exact: private PID/mount/network/cgroup namespaces, `/dev/fuse`,
`CAP_SYS_ADMIN`, container-local AppArmor profile exemption, tmpfs `/run`,
`/run/lock`, `/tmp` and `/sys/fs/cgroup`, and **only** its synthetic task-artifact
directory bound as `/fixture`. It explicitly mounts cgroup2 in the private
namespace before starting systemd, verifies actual filesystem type and manager
readiness, and prepares only the private `ubuntu` user manager. There is no
privileged/all-device mode, host root/HOME/service/Docker socket/cgroup bind or
host cgroup/security/service change. The host's existing inactive `unarr.service`
is compared before/after and is never installed, started or replaced. Cleanup
stops/removes only the named fixture container; logs/provenance survive failures.
