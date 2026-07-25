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
# otherwise leave HOME=/root, and gosu does not set it).
export HOME=/home/unarr

if [ "$(id -u)" = "0" ]; then
	# /downloads is chowned NON-recursively on purpose: it is the user's media
	# library and can hold terabytes — a recursive chown on every start would
	# stall the container for minutes. Only the mount point itself needs to be
	# writable; unarr creates its own files as PUID.
	chown "$PUID:$PGID" /downloads 2>/dev/null || true
	for d in /config /data /home/unarr; do
		[ -d "$d" ] || continue
		chown -R "$PUID:$PGID" "$d" 2>/dev/null || true
	done

	# gosu passes signals straight through and execs, so the daemon stays PID 1's
	# child and `docker stop` still shuts it down gracefully.
	exec gosu "$PUID:$PGID" unarr "$@"
fi

# Already unprivileged (docker run --user ...) — nothing to drop, and the chown
# above would fail anyway.
exec unarr "$@"
