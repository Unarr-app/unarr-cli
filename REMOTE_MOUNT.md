# Remote library: web accounts, local mount

The remote library is read-only, optional and **disabled by default**.
It requires an active paid plan (including PRO+ and paid lifetime access).
Free accounts, free trials and expired plans cannot use it. The web profile's
Agents section shows a purchase CTA or setup instructions for the current plan.
Startup checks the web entitlement before opening the listener or cache. While
running, the CLI rechecks every 30 seconds and closes the service if verification
fails (including network loss; a request has a ten-second timeout).
Provider accounts are configured once on the Unarr website. Their API keys stay
encrypted in the existing web database; the CLI never receives debrid API keys.
There is no separate local provider configuration or encryption key file.

## Setup

1. Connect your Real-Debrid, AllDebrid, TorBox or Torrin account in the existing
   Unarr web settings. Configure Usenet there too if you want direct NZB access.
2. Run `unarr login` to link this device to that Unarr account.
3. Install [rclone](https://rclone.org/install/) and FUSE on Linux, macFUSE on
   macOS, or WinFsp on Windows.
4. Run `unarr config mount` (also available as **Mount** in `unarr config`).
   Enable the mount and optionally choose a local NZB directory.
5. Create an empty directory and run `unarr mount /path/to/remote-media`.

On Windows, use an unused drive letter (`unarr mount X:`) or a nonexistent
directory below an existing parent. Linux/macOS use an existing empty directory.
The command starts a loopback WebDAV service and rclone. Ctrl-C or SIGTERM on
Unix stops it and unmounts. The normal download daemon does not start this
separate experimental service.

The complete local settings are:

```toml
[mount]
enabled = false                 # set true explicitly, or use unarr config mount
listen = "127.0.0.1:11820"        # loopback only
refresh_interval = "1m"          # minimum 10s
# cache_dir = "/absolute/path/to/private-metadata"
# nzb_dir = "/absolute/path/to/nzbs"
```

No provider names, tokens, NNTP host/password or duplicate WebDAV password are
stored in this section. Device authentication continues to use Unarr's existing
login mechanism. The local WebDAV credentials are derived in memory from that
login using the existing credential derivation. Local paths and listener changes
require restarting the mount; web account changes are discovered on refresh.

Example tree:

```text
remote-media/
  debrid/real-debrid/Release [id]/video.mkv
  debrid/alldebrid/Season [id]/episode.mkv
  debrid/torbox/Release [id]/video.mkv
  debrid/torrin/Release [id]/video.mkv
  usenet/NZB filename [stable-id]/video.mkv
```

## Web/CLI contract

The matching web backend must be deployed before this CLI feature can work.
The backend includes the paid-access gate and the web profile configuration.
Deploy that backend before publishing the CLI release.

All mount endpoints reuse the existing agent authentication and derive the
user from the authenticated key, never a caller-supplied user ID:

- `GET /api/internal/agent/mount/access`: validates the current paid plan at
  startup and every 30 seconds while mounted.
- `GET /api/internal/agent/mount/accounts`: connected supported accounts and
  opaque account revisions, without raw or encrypted provider credentials.
- `GET /api/internal/agent/mount/library`: paginated metadata and signed file
  references. The backend reuses the existing debrid clients, transport and
  concurrency controls.
- `POST /api/internal/agent/mount/resolve`: validates the reference, its owner
  and current account revision, rechecks the exact file and resolves its URL.
  The media bytes then flow directly from the CDN to the CLI.

References cannot be moved between users or replayed against a replaced account.
Removing an account removes its entries after a successful refresh. Previously
resolved CDN URLs and active readers can remain usable until upstream expiry;
the mount does not claim immediate revocation of those already-issued URLs.

For local NZBs, the CLI uses `/api/internal/agent/mount/usenet-credentials`,
which adds the paid-plan gate to the existing Usenet credential resolver.
NNTP needs the credentials on the device to connect directly: they are fetched
lazily, retained only in memory, refreshed after five minutes on subsequent
article access, and never written to the mount configuration or catalog.
This endpoint retains the existing web-side Usenet entitlement checks.

## Continuity and limits

Metadata snapshots and a private local database keep listings available during
outages and after restart. Failed refreshes retain known entries and can publish
safe partial progress. Account removals apply after a successful complete scan.
The cache defaults to `remote-library/` beside the selected config, honoring
`--config`. It contains metadata and signed references, not media or API keys.

Stat, HEAD, directory listings and seek bookkeeping do not contact the CDN or
NNTP provider. WebDAV listings have a generation-aware cache bounded to 16 MiB
and 256 responses. Independent readers fetch byte ranges on demand, validate
upstream ranges/lengths, and share lazy URL resolution and renewal.

The server admits 32 concurrent GET requests. Built-in rclone uses a 4 MiB buffer
per open file, 32–128 MiB read chunks, a 15-second directory cache and no full-file
disk cache. Usenet reuses the existing bounded article cache and 16 warm plans.

TorBox enumeration includes torrents, Usenet and web downloads. Torrin uses its
native cursor-paginated jobs API. Real-Debrid and AllDebrid reuse cached file
metadata for unchanged releases. The backend coalesces concurrent metadata
requests and limits retained serialized metadata to 16 MiB / 2,048 entries.

Only the four supported **personal** provider accounts are exposed. Managed
credentials that may belong to shared pools are excluded to prevent exposing
other users' files. Premiumize and other adapters still need account enumeration.
Unsupported compressed/encrypted archives, ambiguous multi-video RARs and zipped
TorBox members are not silently presented as independently streamable files.

Direct Usenet watches a nonrecursive local NZB directory; it is not a web NZB
inbox. Add complete manifests by renaming them to `.nzb`. It supports direct
files (including episodes/subtitles) and the existing uncompressed RAR4/RAR5
streaming paths. The original download workflow remains available for other
archives.

## Service-only and containers

`unarr mount serve` starts the authenticated loopback WebDAV endpoint without
rclone. Use the existing derived Unarr WebDAV credentials with an independently
managed rclone remote. Only one mount process may own a given catalog/listener.

In containers, server and rclone must share a network namespace; host-visible
FUSE additionally requires the device, permissions and mount propagation.
The project's default Docker setup does not supply these automatically.

This is the local-folder use case, not full Zurg feature parity: automatic
torrent repair/re-add, provider writes, virtual rename and Arr download-client
emulation are outside this feature. No real-provider comparison with Zurg has
been demonstrated. See [performance and validation](REMOTE_MOUNT_PERFORMANCE.md).

If you tried the earlier, unreleased local-provider configuration, reconnect
those accounts on the web and remove `mount.providers`, `mount.usenet` and
local mount credentials from TOML. Those keys are now unknown settings.
`unarr config encrypt` and the experimental local key-file format were removed;
do not carry its encrypted fields into the standard login configuration.
