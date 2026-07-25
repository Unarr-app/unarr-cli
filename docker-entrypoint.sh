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

if [ "$(id -u)" = "0" ]; then
	if [ "$PUID" = "0" ]; then
		# Root by explicit request. Everything unarr writes will be owned by root,
		# including files on mounts shared with other users — say so once, loudly,
		# so it is never a surprise later.
		echo "unarr: PUID=0 — running as root by explicit request." >&2
		exec unarr "$@"
	fi

	# Without gosu we cannot drop privileges, and running the agent as root
	# anyway would create root-owned files on mounts the host user must be able
	# to manage — the exact permission dead end this script exists to prevent
	# (and what Kubernetes runAsNonRoot admission is protecting against). Refuse
	# instead of silently doing the wrong thing.
	if ! command -v gosu >/dev/null 2>&1; then
		echo "unarr: refusing to run as root — gosu is missing, so privileges cannot be dropped to $PUID:$PGID." >&2
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

	# gosu passes signals straight through and execs, so the daemon stays PID 1's
	# child and `docker stop` still shuts it down gracefully.
	#
	# NOTE: a uid:gid spec makes gosu set exactly that one group — supplementary
	# groups (e.g. a NAS "media" gid granted with --group-add) are NOT carried
	# over. When you need them, start the container unprivileged instead:
	# `--user PUID:PGID --group-add <gid>` takes the branch below, which execs
	# without touching the group list. Documented in DOCKERHUB.md.
	#
	# HOME is re-applied through `env` because gosu derives it from the target
	# uid's passwd entry — and a NAS PUID (1026, 99, …) has none, so it would
	# land on HOME=/ and scatter dotfiles at the filesystem root. `env` execs, so
	# signal passthrough is unaffected.
	exec gosu "$PUID:$PGID" env "HOME=$HOME" unarr "$@"
fi

# Already unprivileged (docker run --user … / Kubernetes runAsUser) — nothing to
# drop, the chown above would fail anyway, and the process keeps whatever
# supplementary groups the runtime gave it.
exec unarr "$@"
