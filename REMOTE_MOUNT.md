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
2. Run `unarr init` to configure the normal agent, including its download
   directory and registered device identity. Then run `unarr mount`. On first
   use, enable the optional feature. Missing/expired
   device authentication opens the existing browser sign-in flow; a valid
   session is reused.
3. unarr checks paid access, reuses compatible dependencies and downloads a
   pinned, SHA-256-verified official rclone into its private tools directory.
   Before installing a missing FUSE/macFUSE/WinFsp driver, it explains why it is
   needed and any system password, approval or restart, then asks permission.
4. The default folder is `~/unarr-media` on Linux/macOS; Windows chooses a free
   drive letter. A custom destination is `unarr mount /path/to/remote-media`.
   The command saves the enabled intention after validation and dependency
   setup, then requests installation or restart of the normal agent service.
   Its success message confirms configuration and that activation was requested;
   check the folder and agent logs to confirm actual mount readiness. The agent
   owns the mount after the terminal closes and retries after login/reboot.
   Run `unarr umount` (or `unarr unmount`) to disable it. `unarr config mount`
   remains available for advanced local settings.

On Windows, use an unused drive letter (`unarr mount X:`) or a nonexistent
directory below an existing parent. Linux/macOS create a missing directory and
reject nonempty destinations. Bare ASCII drives `A:` through `Z:` are accepted;
occupied drives and drive-relative paths such as `X:media` are rejected.
Existing files are never replaced.

Windows mounts require WinFsp 1.9 or newer. The verified installer provides 2.1;
existing installations below 1.9 are unsupported and must be updated before
mounting. The Windows mount uses protected owner read/execute permissions to
deny creation, writes, renames and deletion while allowing media reads and
directory listing. This permission behavior was validated with WinFsp 2.1;
older drivers were not tested.

The agent starts a loopback WebDAV service and rclone. They share the agent's
lifecycle and are retried after transient failure. Stopping the whole agent also
stops the mount; starting it again restores an enabled mount.

Persistent `mount` and `umount` use the one default agent service and its default
config: `~/.config/unarr/config.toml` on Linux,
`~/Library/Application Support/unarr/config.toml` on macOS, and
`%APPDATA%/unarr/config.toml` on Windows. A different `--config`,
`UNARR_CONFIG_DIR`, shell-only `UNARR_API_KEY`, `UNARR_API_URL`,
`UNARR_DOWNLOAD_DIR`, `UNARR_COUNTRY`, `UNARR_TELEMETRY`, or Linux
`XDG_CONFIG_HOME`/`XDG_DATA_HOME` override is refused before authentication,
configuration writes or service operations. Save these settings in the default
file instead. `mount serve --config /path/to/config.toml` remains available for
an independently managed process. `umount` preserves stopped and parked service
intent; it requests a restart only for a running installed service.
Saving a disabled mount setting does not apply it to an existing mount session.
Run `unarr umount` to unmount that session even when the saved setting is already off.

Each running mount uses an immutable snapshot of the agent's current key and
identity. A minted key, revocation or replacement sign-in cancels and joins the
old DAV, catalog, readers and rclone before starting another session. The agent
also checks saved identity changes every five seconds while mounting. Removing
the identity stops mounting until a new identity is available. Daemon exits
cancel and wait for mount cleanup with a fifteen-second bound; rclone is given
five seconds to exit before being terminated.

Dependency preparation never runs while disabled. The normal daemon may validate
or reuse the private rclone binary, but noninteractive sessions cannot approve
system installations or enable the feature.
On supported Linux distributions unarr uses apt/dnf/yum/pacman/zypper/apk and
sudo/doas if needed. macOS uses the verified official macFUSE installer; Windows
installs the default WinFsp components with a progress indicator after explaining
the installation and requesting consent. Native UAC prompts remain, and automatic
restarts are disabled. macOS security approval/recovery changes are never automated.
Unsupported distributions and containers without host FUSE access get actionable
instructions. `mount serve` needs neither rclone nor a filesystem driver.

Older rclone installations are reused only when they support all required mount
options. Incompatible versions get a private verified replacement; the user's
existing executable is preserved. Inactive macFUSE requires explained activation
and any native security approval. On Arch, installing missing FUSE also performs
a full system upgrade to avoid a partial upgrade; this is explicitly disclosed
before consent. See [the test matrix](REMOTE_MOUNT_TEST_MATRIX.md) for native
results and remaining platform limitations.

The complete local settings are:

```toml
[mount]
enabled = false                 # set true explicitly, or use unarr config mount
directory = "/home/me/unarr-media" # persisted by unarr mount
listen = "127.0.0.1:11820"        # loopback only
refresh_interval = "1m"          # minimum 10s
# cache_dir = "/absolute/path/to/private-metadata"
# nzb_dir = "/absolute/path/to/nzbs"
```

No provider names, tokens, NNTP host/password or duplicate WebDAV password are
stored in this section. Device authentication continues to use Unarr's existing
login mechanism. The local WebDAV credentials are derived in memory from that
login using the existing credential derivation. Local paths and listener changes
require restarting the agent; web account changes are discovered on refresh.

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

For Usenet NZBs, the CLI uses `/api/internal/agent/mount/usenet-credentials`,
which adds the paid-plan gate to the existing Usenet credential resolver.
NNTP needs the credentials on the device to connect directly: they are fetched
lazily, retained only in memory, refreshed after five minutes on subsequent
article access, and never written to the mount configuration or catalog.
This endpoint retains the existing web-side Usenet entitlement checks. The web
action **Add to local Usenet folder** dispatches only the selected NZB manifest
to a compatible online agent (reserved future CLI floor: 1.17.0), which stores it
in the managed `mount-nzbs` inbox beside `config.toml`. If there is no agent,
the web shows the install CTA; an older agent gets the update CTA. Copying a complete `.nzb` file
into the same inbox manually is also supported.
Remote mounting is not released; CLI 1.17.0 is reserved and unpublished.
The published 1.16.0/1.16.1 agents do not support this feature.

## Continuity and limits

Metadata snapshots and a private local database keep listings available during
outages and after restart. Failed refreshes retain known entries and can publish
safe partial progress. Account removals apply after a successful complete scan.
Mount setup, paid-access checks and source requests use the existing configured
API mirrors for transient failures. A paid-access denial is not retried through
another mirror. Missing files in a direct NZB do not hide healthy siblings;
partial manifests are retried on later refreshes while retaining known entries.
The cache defaults to `remote-library/` beside the selected config, honoring
`--config`. It contains metadata and signed references, not media or API keys.

Access probes give each mirror attempt three seconds, leaving time for a
healthy mirror inside the ten-second access watchdog. Metadata and URL resolution
use a finite thirty-second budget per mirror attempt to cover cold provider
pacing and queued resolution; caller cancellation can shorten it. 403/410 denials
are terminal. Library pages keep the `entries`/`next` envelope and
`path`/`key`/`size`/`reference` entry fields; `next` is an opaque signed cursor,
including when a single release spans pages. The serialized response limit is
1048576 bytes inclusive; larger responses fail explicitly without including
their body in the error.

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

Direct Usenet watches the nonrecursive managed NZB inbox. The website can add
manifests through the agent, or you can add complete files manually by renaming
them to `.nzb`. It supports direct files (including episodes/subtitles) and the
existing uncompressed RAR4/RAR5 streaming paths. The original download workflow
remains available for other archives.

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
