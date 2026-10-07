#!/usr/bin/env bash
set -euo pipefail

# Run against a task-specific source snapshot and native-built binaries.
# No driver installer, security-policy change, global launchd env or reboot.
if [[ $# -ne 7 ]]; then
  echo 'usage: remote-mount-native.sh SOURCE_SHA TASK_TAG CLI CMD_TEST SETUP_TEST NNTP_TEST YENC_TEST' >&2
  exit 2
fi
sha=$1 tag=$2 cli=$3 cmd_test=$4 setup_test=$5 nntp_test=$6 yenc_test=$7
[[ $sha =~ ^[a-f0-9]{40}$ && $tag =~ ^[a-zA-Z0-9_-]+$ ]] || exit 2
for binary in "$cli" "$cmd_test" "$setup_test" "$nntp_test" "$yenc_test"; do [[ -x $binary ]] || exit 2; done
artifact="${TMPDIR:-/tmp}/unarr-native-${tag}-${sha}"
[[ ! -e $artifact ]] || { echo 'fresh task/SHA artifact directory required' >&2; exit 2; }
mkdir -m 700 "$artifact"
export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"
export GOMAXPROCS=2 UNARR_NO_TELEMETRY=1 UNARR_TELEMETRY=off
export UNARR_NATIVE_ACCEPTANCE=1 UNARR_NATIVE_CLI="$cli"
export UNARR_NATIVE_TOOLS_DIR="$artifact/tools" UNARR_NATIVE_PREPARE_RCLONE=1
export UNARR_LAUNCHD_E2E=1
# Kernel opt-in additionally requires the already-approved, loaded extension.
# The harness never tries to activate it. Leave UNARR_NATIVE_KERNEL unset/0
# while human approval is pending.
if [[ ${UNARR_NATIVE_KERNEL:-0} == 1 ]]; then
  kmutil showloaded --list-only 2>/dev/null | grep -q 'io.macfuse.filesystems.macfuse' || {
    echo 'macFUSE is not loaded; human approval/activation remains an external prerequisite' >&2
    exit 2
  }
fi
{
  echo "SOURCE_SHA=$sha"
  echo "TASK=$tag"
  echo 'NATIVE_EXECUTION=darwin/arm64; cross-compilation alone is not acceptance'
  sw_vers
  uname -m
  shasum -a 256 "$cli" "$cmd_test" "$setup_test" "$nntp_test" "$yenc_test"
  "$cli" --config "$artifact/nonexistent-version-config.toml" version
} > "$artifact/provenance.txt" 2>&1
"$cmd_test" -test.v -test.run '^TestMountNativeMacFixtureGuard$' -test.timeout 30s > "$artifact/guard-before.txt" 2>&1
"$setup_test" -test.v -test.run '^TestNativePrepareRclone$' -test.timeout 6m > "$artifact/setup.txt" 2>&1
export UNARR_NATIVE_RCLONE="$UNARR_NATIVE_TOOLS_DIR/rclone-1.75.1-darwin-arm64/rclone"
"$UNARR_NATIVE_RCLONE" version >> "$artifact/provenance.txt" 2>&1
set +e
"$cmd_test" -test.v -test.run '^(TestMountNative|TestLaunchdReal(Lifecycle|DisabledAndCrashRecovery|RejectsCrashLoop|PreservesUserDomain))' -test.timeout 5m > "$artifact/native.txt" 2>&1
status=$?
set -e
echo "EXIT=$status" >> "$artifact/native.txt"
set +e
"$nntp_test" -test.v -test.run '^Test(Boundary(CancelInFlight|CancelDuringRepairHandshake|EncodedBodyCeiling|SmallReceiveLimits|OversizeRetiresAndRecovers|IncompleteWireRetiresAndRetries)|Final(OversizedReplyPublicAPI|ReplyCeilingPublicAPI|ReplyContinuationsCannotBeCleanAlternatives|ReplyFramingAndBufferedBytes)|TimedOutConnectionIsNeverReused|MidBodyResetsNeverLeakPoolSlots)$' -test.timeout 90s > "$artifact/nntp.txt" 2>&1
nntp_status=$?
"$yenc_test" -test.v -test.run '^Test(Decode(SimpleArticle|Multipart|BinaryData)|EncodeDecodeRoundTrip(SinglePart|Multipart)|BoundaryYEncIntegrity|Final(HeaderFieldsExcludeFilename|ExactMultipartHeaderFields))$' -test.timeout 90s > "$artifact/yenc.txt" 2>&1
yenc_status=$?
set -e
echo "EXIT=$nntp_status" >> "$artifact/nntp.txt"
echo "EXIT=$yenc_status" >> "$artifact/yenc.txt"
if [[ $nntp_status -ne 0 || $yenc_status -ne 0 ]]; then status=1; fi
"$cmd_test" -test.v -test.run '^TestMountNativeMacFixtureGuard$' -test.timeout 30s > "$artifact/guard-after.txt" 2>&1 || status=1
echo "ARTIFACT=$artifact"
exit "$status"
