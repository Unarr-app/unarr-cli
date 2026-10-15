# `GET /sessions` — schema v1

Served by the daemon's loopback control plane (`127.0.0.1:<controlPort>`, header
`X-Unarr-Control-Token`; both in the daemon state file). Read-only, `GET` only.
The same document is printed by `unarr status --sessions --json`.

* `404` — the daemon predates `/sessions`. `401` — bad token. `405` — not GET.
* **Compatibility:** additive changes (new optional fields, new `kind`/`state`
  values) keep `schemaVersion: 1`. Consumers must ignore unknown fields and
  unknown `kind`/`state` strings. Removing or re-typing a field bumps
  `schemaVersion`.
* All maps and arrays are always present (`[]` / `{}`, never `null`). Optional
  scalars are omitted, not `null`.

## Example

```json
{
  "schemaVersion": 1,
  "generatedAt": "2026-10-13T10:15:30Z",
  "agent": { "version": "1.18.2", "uptimeSec": 86400 },
  "sessions": [
    {
      "id": "3f2a9c1e-…",
      "kind": "download",
      "title": "Some.Show.S01E01.1080p",
      "state": "downloading",
      "health": "ok",
      "startedAt": "2026-10-13T10:00:00Z",
      "activeSec": 930,
      "bytes": 1503238553,
      "rateBps": 5242880,
      "avgRateBps": 1616000,
      "progress": 42,
      "stall": { "count": 0, "active": false }
    },
    {
      "id": "hls-8b1d…",
      "kind": "hls",
      "title": "Some.Movie.2160p.mkv",
      "state": "active",
      "health": "stalled",
      "startedAt": "2026-10-13T10:10:00Z",
      "activeSec": 330,
      "bytes": 88000000,
      "rateBps": 120000,
      "avgRateBps": 266666,
      "speedX": 0.62,
      "stall": { "count": 1, "active": true, "lastAt": "2026-10-13T10:14:10Z", "lastReason": "transcode" }
    }
  ],
  "summary": {
    "active":           { "download": 1, "hls": 1, "stream": 0, "mount": 0 },
    "stalled": 1,
    "bytesTotal":       { "download": 912345678, "hls": 88000000, "stream": 0, "mount": 0 },
    "stallsTotal":      { "download": 0, "hls": 1, "stream": 0, "mount": 0 },
    "rateBps":          { "download": 5242880, "hls": 120000, "stream": 0, "mount": 0 },
    "downloadsByState": { "downloading": 1, "paused": 2 },
    "vpn": { "enabled": true, "required": true, "up": true }
  }
}
```

## Top level

| Field | Type | Notes |
|-------|------|-------|
| `schemaVersion` | int | `1` |
| `generatedAt` | RFC 3339 UTC | when the snapshot was taken |
| `agent.version` | string | CLI version |
| `agent.uptimeSec` | int | seconds since the daemon started |
| `sessions` | `Session[]` | sorted by kind (`download`, `hls`, `stream`, `mount`), then `startedAt` |
| `summary` | `Summary` | aggregates, see below |

## `Session`

| Field | Type | Notes |
|-------|------|-------|
| `id` | string | task id (`download`, `stream`), HLS session id (`hls`), `"mount"` (`mount`). Stable for the life of the session. |
| `kind` | `"download"` \| `"hls"` \| `"stream"` \| `"mount"` | `hls` = in-browser transcode/copy playback; `stream` = direct single-file stream with a reader attached; `mount` = the WebDAV remote folder (one aggregate session, present only while a read is in flight) |
| `title` | string | display title; may be empty (`stream` without a known task, …) |
| `state` | string | `download`: the manager status (`resolving`, `downloading`, `verifying`, `organizing`, `seeding`, …). `hls`: `active` (viewer fetched in the last 30 s) or `idle`. `stream`, `mount`: `active`. |
| `health` | `"ok"` \| `"degraded"` \| `"stalled"` | `degraded` = impaired for less than 30 s; `stalled` = impaired for ≥ 30 s; back to `ok` after 10 s of healthy flow |
| `startedAt` | RFC 3339 UTC | session start; when the source does not know it, the first time the daemon sampled it (≤ 5 s late) |
| `activeSec` | int | seconds since `startedAt` |
| `bytes` | int | bytes moved by the session: downloaded (`download`), written to viewers (`hls`, `mount` — mount is the process-wide total). `0` when not counted (`stream`) |
| `rateBps` | int | current rate, bytes/s (the torrent/debrid speed for downloads; delta between samples otherwise) |
| `avgRateBps` | int | `bytes / activeSec` |
| `speedX` | number, optional | encoder speed in ×realtime (1.0 = realtime). Only `hls` with live transcode telemetry; absent for copy/cache replays |
| `progress` | int 0–100, optional | `download` only |
| `stall` | `Stall` | |

### What counts as "impaired"

| Kind | Impaired when |
|------|---------------|
| `download` | status `downloading` and speed is 0 (reason `no_data`) |
| `hls` | the encoder is `struggling` — the same verdict the web gets (`< 0.75×` realtime, or input-bound below `0.95×`) — or the encoder gave up (`encoder_failed`). `marginal` (0.75–0.95×) is **not** impaired. Idle sessions and cache replays are never impaired. |
| `mount` | a read is in flight and no bytes were served since the previous sample (`no_data`) |
| `stream` | never (no flow signal available) |

## `Stall`

| Field | Type | Notes |
|-------|------|-------|
| `count` | int | stall **episodes** of this session — one per stall, not per sampling tick |
| `active` | bool | in a stall right now (`health == "stalled"`) |
| `lastAt` | RFC 3339 UTC, optional | when the latest stall began |
| `lastReason` | string, optional | `no_data`, `transcode`, `input_bound`, `encoder_failed` |

## `Summary`

| Field | Type | Notes |
|-------|------|-------|
| `active` | `{kind: int}` | sessions per kind; every kind present |
| `stalled` | int | sessions with `health == "stalled"` now |
| `bytesTotal` | `{kind: int}` | monotonic since daemon start; keeps counting after sessions end |
| `stallsTotal` | `{kind: int}` | monotonic stall episodes since daemon start |
| `rateBps` | `{kind: int}` | sum of current rates |
| `downloadsByState` | `{state: int}` | every known download, running or only persisted (`downloading`, `paused`, `stopped`, `queued`, …) |
| `vpn.enabled` / `vpn.required` / `vpn.up` | bool | managed VPN configured / kill-switch on / tunnel healthy (handshake fresh) |

## Sampling

A daemon goroutine polls every source every 5 s; that tick feeds the stall
monitor. Each `GET /sessions` (and `/metrics` scrape) also takes a fresh sample,
which never double-counts a stall — the monitor works on timestamps. Rates
derived from byte deltas are recomputed at most once per second.
