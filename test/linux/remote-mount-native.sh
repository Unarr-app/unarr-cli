#!/usr/bin/env bash
set -euo pipefail

# Actual CLI/systemd persistence in private native namespaces, never the host
# unarr.service. Binaries must be built from the same recorded candidate SHA.
if [[ $# -ne 5 ]]; then
  echo 'usage: remote-mount-native.sh SOURCE_SHA TASK_TAG CLI CMD_TEST VERIFIED_RCLONE' >&2
  exit 2
fi
sha=$1 tag=$2 cli=$3 cmd_test=$4 rclone=$5
[[ $sha =~ ^[a-f0-9]{40}$ && $tag =~ ^[a-zA-Z0-9_-]+$ ]] || exit 2
for binary in "$cli" "$cmd_test" "$rclone"; do [[ -x $binary ]] || exit 2; done
[[ $(basename "$(dirname "$rclone")") == rclone-1.75.1-linux-amd64 ]] || exit 2
[[ -c /dev/fuse ]] || { echo 'existing /dev/fuse required; no host installation' >&2; exit 2; }
artifact=$(mktemp -d "/tmp/unarr-native-${tag}-${sha}-linux.XXXXXX")
name="unarr-native-linux-${tag}-${sha:0:12}"
prep="${name}-prepare"
image="${UNARR_NATIVE_FIXTURE_IMAGE:-unarr-native-systemd:${tag}-${sha:0:12}}"
owned_image=0 started=0 prepared=0
host_before=$(systemctl --user show unarr.service -p LoadState -p ActiveState -p SubState -p MainPID)
cleanup() {
  status=$?
  trap - EXIT INT TERM
  if (( started )); then
    docker exec "$name" journalctl --no-pager -n 100 > "$artifact/container-journal.txt" 2>&1 || true
    docker logs "$name" > "$artifact/container-boot.txt" 2>&1 || true
    docker stop --timeout 10 "$name" > "$artifact/container-stop.txt" 2>&1 || status=1
    docker rm "$name" >> "$artifact/container-stop.txt" 2>&1 || status=1
  fi
  if (( prepared )); then docker rm -f "$prep" >> "$artifact/container-stop.txt" 2>&1 || status=1; fi
  if (( owned_image )); then docker image rm "$image" >> "$artifact/container-stop.txt" 2>&1 || status=1; fi
  host_after=$(systemctl --user show unarr.service -p LoadState -p ActiveState -p SubState -p MainPID)
  if [[ $host_after != "$host_before" ]]; then echo 'HOST_UNIT_CHANGED=1' >> "$artifact/result.txt"; status=1; fi
  printf '%s\n' "$host_after" > "$artifact/host-unit-after.txt"
  echo "EXIT=$status" >> "$artifact/result.txt"
  echo "ARTIFACT=$artifact"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
mkdir -m 755 "$artifact/bin" "$artifact/tools" "$artifact/tools/rclone-1.75.1-linux-amd64"
install -m 755 "$cli" "$artifact/bin/unarr"
install -m 755 "$cmd_test" "$artifact/bin/cmd-native.test"
install -m 755 "$rclone" "$artifact/tools/rclone-1.75.1-linux-amd64/rclone"
# Only synthetic executable/log artifacts are shared, not the host HOME.
chmod 755 "$artifact"
{
  echo "SOURCE_SHA=$sha"
  echo "TASK=$tag"
  echo 'NATIVE_EXECUTION=linux/amd64 private container on native host kernel'
  sha256sum "$artifact/bin/unarr" "$artifact/bin/cmd-native.test" "$rclone"
  uname -srmo
  printf '%s\n' "$host_before"
} > "$artifact/provenance.txt"
if [[ -z ${UNARR_NATIVE_FIXTURE_IMAGE:-} ]]; then
  docker image inspect "$image" >/dev/null 2>&1 && { echo 'fresh fixture image tag required' >&2; exit 2; }
  docker run --detach --name "$prep" --pull never --cgroupns private --entrypoint /bin/sleep ubuntu:24.04 infinity > "$artifact/preparation-container.txt"
  prepared=1
  docker exec "$prep" /bin/sh -c 'apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends systemd dbus dbus-user-session fuse3 ca-certificates sudo' > "$artifact/packages.txt" 2>&1
  docker commit "$prep" "$image" > "$artifact/image.txt"
  owned_image=1
  docker rm -f "$prep" >> "$artifact/preparation-container.txt"
  prepared=0
else
  docker image inspect "$image" > "$artifact/image.json"
fi
docker run --detach --tty --name "$name" --pull never --cgroupns private \
  --device /dev/fuse --cap-add SYS_ADMIN --security-opt apparmor=unconfined \
  --env SYSTEMD_LOG_TARGET=console --tmpfs /run --tmpfs /run/lock --tmpfs /tmp \
  --tmpfs /sys/fs/cgroup:rw --mount "type=bind,src=$artifact,dst=/fixture" \
  --entrypoint /bin/sh "$image" \
  -c 'mount -t cgroup2 none /sys/fs/cgroup && stat -f -c %T /sys/fs/cgroup && exec /sbin/init --unit=multi-user.target' > "$artifact/container-id.txt"
started=1
ready=0
for _ in {1..80}; do
  if docker exec "$name" systemctl is-system-running >/dev/null 2>&1; then ready=1; break; fi
  sleep 0.1
done
(( ready )) || { docker logs "$name" >&2; exit 1; }
docker exec "$name" /bin/sh -c 'stat -f -c %T /sys/fs/cgroup; systemctl is-system-running; systemctl show -p Version; uname -srmo; dpkg-query -W systemd fuse3 dbus; id ubuntu' > "$artifact/manager.txt"
docker exec "$name" loginctl enable-linger ubuntu
ready=0
for _ in {1..80}; do
  if docker exec --user ubuntu --env HOME=/home/ubuntu --env XDG_RUNTIME_DIR=/run/user/1000 "$name" \
    systemctl --user is-system-running >> "$artifact/manager.txt" 2>&1; then ready=1; break; fi
  sleep 0.1
done
(( ready )) || { echo 'private user-manager readiness timeout' >&2; exit 1; }
docker exec --user ubuntu --env HOME=/home/ubuntu --env XDG_RUNTIME_DIR=/run/user/1000 \
  --env GOMAXPROCS=2 --env UNARR_NATIVE_ACCEPTANCE=1 --env UNARR_NATIVE_KERNEL=1 \
  --env "UNARR_NATIVE_DIAGNOSTIC=${UNARR_NATIVE_DIAGNOSTIC:-0}" \
  --env UNARR_NATIVE_LINUX_SERVICE=1 --env UNARR_NATIVE_CLI=/fixture/bin/unarr \
  --env UNARR_NATIVE_RCLONE=/fixture/tools/rclone-1.75.1-linux-amd64/rclone \
  "$name" /fixture/bin/cmd-native.test -test.v -test.run '^TestMountNativeLinuxPersistent$' -test.timeout 4m > "$artifact/result.txt" 2>&1
docker exec --user ubuntu --env HOME=/home/ubuntu --env XDG_RUNTIME_DIR=/run/user/1000 "$name" \
  /bin/sh -c 'systemctl --user show unarr.service -p LoadState -p ActiveState -p MainPID; pgrep -x rclone || test $? = 1' > "$artifact/user-cleanup.txt"
