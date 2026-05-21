# Phase 2.2 — Per-task stream token (deferred)

Status: deferred. Requires coordinated change in the web app
(`torrentclaw-web`) and the CLI daemon. Pulled out of the Phase 2
security pass because the CLI-only fixes (UPnP opt-in, SSE caps,
signed self-update) ship without web-side work; the stream-token
work cannot.

## Problem

`/stream`, `/playlist.m3u` and `/hls/<sessionID>/...` on the daemon
HTTP server have no authentication. Today, anyone who can reach the
listener and guesses (or learns) the `taskID` (for `/stream`) or
`sessionID` (for `/hls`) can fetch the active file.

Mitigations already in place after Phase 1+2:

- `sessionID` is restricted to a safe regex and is a server-issued
  UUID v4 (122-bit entropy, not enumerable in practice).
- `/health` no longer leaks the active filename, taskID prefix or
  client IP to remote callers (loopback diagnostics preserved).
- UPnP is opt-in, so by default the daemon is not exposed to the
  public internet.
- The web client probes `/health` to pick LAN vs Tailscale.

Residual risk:

- On a shared LAN (open Wi-Fi, office network, dorm) any device can
  reach the listener and brute-force `?id=<taskID>` against
  `/stream`. taskIDs are also UUIDs, so this is high entropy, but
  the URL may leak through browser history, sharing, screen capture
  or a passive logger and there is no second factor.
- A user who explicitly opts into UPnP exposes the same surface to
  the entire internet.

A per-task secret carried in the URL closes this without breaking
the `<video src>` flow (the browser cannot attach `Authorization`
headers to media elements, but it can append a query parameter).

## Design

Both ends agree on a per-task secret token. The web generates it
when the user requests streaming; the daemon receives the
`(taskID, token)` pair and validates the token on every `/stream`
and `/hls/...` request.

### Web side (`torrentclaw-web`)

When the user clicks "Stream":

1. Generate `streamToken = crypto.randomBytes(32).toString("hex")`
   server-side (NOT browser, so it never lives in client storage
   longer than the page lifetime).
2. Persist `(taskID, streamToken, expiresAt)` in `download_task`
   (new columns or a sibling table). Token expires e.g. 6 h after
   issue or on explicit revoke.
3. Push the token to the daemon over the existing heartbeat / sync
   channel that already carries `streamRequested`. Add a
   `streamToken` field next to it. The daemon trusts that channel
   (it is authenticated agent ↔ origin).
4. Include the token in the stream URLs the API returns to the
   browser:
   `http://<host>:<port>/stream?id=<taskID>&t=<streamToken>` and
   the `/hls/<sessionID>` URLs gain `?t=<streamToken>` too.

Files that will need to change:

- `src/lib/services/agent.ts` — extend the stream-request payload
  with `streamToken`.
- `src/lib/db/schema.ts` — column / table for the token.
- `src/lib/services/stream-resolve.ts` — append `&t=` to the URLs
  it builds.
- `src/lib/stream-probe.ts` — keep probing `/health` (no token),
  then append `&t=` to the winning stream URL before returning.
- `src/middleware.ts` — no CORS change required (browser still hits
  daemon directly).

### CLI side

- `internal/agent/types.go` / `internal/agent/sync.go` — accept and
  store `streamToken` next to `streamRequested`.
- `internal/agent/daemon.go` — when the heartbeat reports a new
  active stream task, push the token into the stream server via a
  setter: `streamSrv.SetTaskToken(taskID, token)`.
- `internal/engine/stream_server.go`:
    - New field `tokens map[string]string` guarded by mutex.
    - `SetTaskToken(taskID, token)` and `ClearTaskToken(taskID)`.
    - `handler` (`/stream`) extracts `?id=` and `?t=`, checks the
      token with `subtle.ConstantTimeCompare`; 404 on mismatch.
    - `hlsHandler` (`/hls/<sessionID>/...`) needs an HLS-session
      → token mapping, since the path carries `sessionID` not
      `taskID`. Store the token on the `HLSSession` at start and
      validate per request.

### Backwards compatibility

- The daemon must accept token-less requests for one minor version
  so a newer daemon can still serve an older web (and vice-versa).
  Gate the check on a config flag (`require_stream_token`,
  default false in the first release, default true in the next).
- The `<video src>` form supports query parameters, so the only
  user-visible change is the URL string.

## Open questions to resolve before implementing

1. Token TTL. 6 h gives plenty of room for a movie + pause +
   resume; longer means the post-leak window is wider.
2. Where to store the token in `download_task` — same row, or a
   sibling `download_stream_token` table that we can rotate
   without writing to the task row.
3. Should `/playlist.m3u` (VLC) embed the token directly, or use
   a one-shot redeem URL? VLC URL ends up in history.
4. Token reuse across HLS reconnects — yes, scoped to the
   `HLSSession`, invalidated on `Close()`.
5. Do we want a daemon flag `--require-stream-token` independent
   of config, for users to flip on quickly without editing TOML?

## Effort estimate

- CLI: ~3 h
- Web: ~3 h
- Migration + rollout (config flag flip): 1 release cycle of soak.

## Why not now

- Cross-repo coordination raises commit blast radius beyond what
  the Phase 2 security pass should carry.
- Web work needs DB migration + UI surfaces (the "stream link
  expired" path).
- Phase 2 hardenings ship value today without it; this is the
  defense-in-depth layer on top.
