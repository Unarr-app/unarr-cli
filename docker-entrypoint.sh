#!/bin/sh
# Drop privileges to PUID:PGID (default 1000:1000) after fixing ownership of the
# mounted volumes.
#
# The image bakes a uid/gid 1000 user, which only matches a typical desktop
# Linux host. NAS platforms — the single most common place unarr runs — do not:
# Synology users start at 1024, unRAID uses 99:100, QNAP varies. There the
# bind-mounted /config and /downloads are owned by a uid the container cannot
# write as, so the agent dies on its first write with permission denied and the
# user has no way to fix it from inside. PUID/PGID is the convention every NAS
# user already knows from linuxserver.io images.
set -e

PUID=${PUID:-1000}
PGID=${PGID:-1000}

# Keep the home directory the image has always used (running as root would
# otherwise leave HOME=/root).
export HOME=/home/unarr

# keep_groups prints, comma-separated, the supplementary gids the agent must keep
# after dropping root: every group the runtime gave this process (--group-add /
# compose group_add) plus the owner group of each GPU node under /dev/dri, so a
# GPU passed with `--device /dev/dri` alone is usable without also looking up
# and adding the host's render gid. gid 0 (root) and PGID itself are left out.
keep_groups() {
	{
		id -G | tr ' ' '\n'
		for dev in /dev/dri/renderD* /dev/dri/card*; do
			[ -e "$dev" ] && stat -c %g "$dev"
		done
	} | grep -vx -e 0 -e "$PGID" | sort -un | paste -sd, -
}

if [ "$(id -u)" = "0" ]; then
	if [ "$PUID" = "0" ]; then
		# Root by explicit request. Everything unarr writes will be owned by root,
		# including files on mounts shared with other users — say so once, loudly,
		# so it is never a surprise later.
		echo "unarr: PUID=0 — running as root by explicit request." >&2
		exec unarr "$@"
	fi

	# Without setpriv we cannot drop privileges, and running the agent as root
	# anyway would create root-owned files on mounts the host user must be able
	# to manage — the exact permission dead end this script exists to prevent
	# (and what Kubernetes runAsNonRoot admission is protecting against). Refuse
	# instead of silently doing the wrong thing.
	if ! command -v setpriv >/dev/null 2>&1; then
		echo "unarr: refusing to run as root — setpriv is missing, so privileges cannot be dropped to $PUID:$PGID." >&2
		echo "  Start the container with --user $PUID:$PGID (runAsUser in Kubernetes), or set PUID=0 to run as root deliberately." >&2
		exit 1
	fi

	# /downloads is chowned NON-recursively on purpose: it is the user's media
	# library and can hold terabytes — a recursive chown on every start would
	# stall the container for minutes. Only the mount point itself needs to be
	# writable; files unarr CREATES are owned by PUID, while files that were
	# already there keep their original owner (fix those on the host with
	# chown -R). See DOCKERHUB.md.
	chown "$PUID:$PGID" /downloads 2>/dev/null || true
	for d in /config /data /home/unarr; do
		[ -d "$d" ] || continue
		chown -R "$PUID:$PGID" "$d" 2>/dev/null || true
	done

	# setpriv execs in place, so the daemon stays PID 1 and `docker stop` still
	# shuts it down gracefully.
	#
	# Supplementary groups MUST survive the drop. gosu (used here until
	# 2026-09) sets exactly one group for a uid:gid spec, which silently threw
	# away the render group granted with `group_add` — /dev/dri/renderD128 became
	# unreadable and every Intel QSV transcode died at encoder open
	# ("Error creating a MFX session: -9"). keep_groups carries over what the
	# runtime granted plus the owner group of the GPU nodes.
	#
	# HOME is re-applied through `env` so a NAS PUID with no passwd entry (1026,
	# 99, …) doesn't land on HOME=/ and scatter dotfiles at the filesystem root.
	extra=$(keep_groups)
	if [ -n "$extra" ]; then
		exec setpriv --reuid="$PUID" --regid="$PGID" --groups="$extra" env "HOME=$HOME" unarr "$@"
	fi
	exec setpriv --reuid="$PUID" --regid="$PGID" --clear-groups env "HOME=$HOME" unarr "$@"
fi

# Already unprivileged (docker run --user … / Kubernetes runAsUser) — nothing to
# drop, the chown above would fail anyway, and the process keeps whatever
# supplementary groups the runtime gave it.
exec unarr "$@"
