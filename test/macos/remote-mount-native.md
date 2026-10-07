# Synthetic native remote mount acceptance

`remote-mount-native.sh` runs native test binaries against an isolated fake paid
API and deterministic ranged CDN. Build the CLI and test binaries for
`internal/cmd`, `internal/mountsetup`, `internal/usenet/nntp` and
`internal/usenet/yenc` from the same recorded commit on the Mac with
`GOMAXPROCS=2 go build -p 2` / `go test -p 2 -c`; cross-compiled binaries are not
native evidence until executed here. Pass absolute binary paths and a fresh task
tag. The script preserves logs and SHA-256 provenance under its printed artifact
directory and prepares only the official checksummed rclone binary.

```bash
bash test/macos/remote-mount-native.sh "$SOURCE_SHA" "$TASK_TAG" \
  "$ARTIFACT_DIR/unarr" "$ARTIFACT_DIR/cmd-native.test" \
  "$ARTIFACT_DIR/mountsetup-native.test" "$ARTIFACT_DIR/nntp-native.test" \
  "$ARTIFACT_DIR/yenc-native.test"
```

Keep `UNARR_NATIVE_KERNEL=0` while macFUSE approval is pending. Kernel tests
explicitly SKIP; DAV recovery and actual `mount serve --config` subprocess
entitlement tests still run. After the coordinator confirms human approval and
the driver is already loaded, `UNARR_NATIVE_KERNEL=1` enables genuine mounted
listing, reads, denied mutations, blocked-read cancellation and remount.

The four `TestLaunchdReal...` controller tests guard **both** canonical/legacy
labels and plists in the real user before creating temporary fixtures. They use
private HOME and fixture jobs, leave launchd global environment untouched and
cannot establish native mount persistence. `mount serve` likewise establishes
foreground CLI/DAV behavior, not persistent service activation. Actual persistent
Mac mounting remains a separate acceptance gate while kernel approval is blocked.

The runner also executes a bounded loopback NNTP socket/cancellation/recovery
and reply/BODY receive subset, plus complete yEnc decoding and filename/integrity
regressions. These run natively even while kernel approval is pending; package
compilation alone is not counted as acceptance.
