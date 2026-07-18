# Plan: `unarr doctor --fix` — self-repair for typical/misconfigured installs

Status: **core + level-2 implemented** (2026-07-15) — product fix + `doctor --fix`
framework + safe config repairs, plus (second pass, audited): agent-registration
repair, config-perms chmod 0600, corrupt-TOML backup+regenerate (always confirmed,
even under `--yes`), API-key 401/403 classification (`classifyAuthError` → guides to
`unarr login`), partial-failure-safe repair loop (a failed repair no longer discards
the applied ones), and the "API reachable" check now labels the discovery host it
actually probes. Secret/heavy repairs (re-auth flow, ffmpeg fetch, ports, service
install) remain guidance-only TODOs (see below).
Owner: CLI

## Shipped in this pass
- **Product fix** — `internal/cmd/discovery_hosts.go`: `discoveryHosts()` routes the
  discovery client (search/stats/popular/recent/inspect/watch) to the TorrentClaw
  entries of `[api_url]+mirrors`, dropping unarr hosts. Wired into `getClient()`
  (`root.go`). Verified: `unarr search`/`stats` work with the `api_url = unarr.app`
  default that previously 404'd. Fixes every installed 1.3.x agent on update — no user
  action needed.
- **`doctor --fix` framework** — `internal/cmd/doctor_fix.go` + flags on the doctor
  command (`--fix`, `--dry-run`, `--yes`). Backs up `config.toml` first, applies safe
  repairs, saves once, idempotent. Implemented repairs: normalize `api_url`
  (scheme/trailing-slash/`/api` suffix), repopulate empty mirror list, set/create the
  download directory.
- **New "Discovery API (search/stats)" check** — probes an actual discovery endpoint so
  plain `unarr doctor` stops being falsely green on a primary that answers `/api/health`
  but 404s the catalog.
- Tests: `discovery_hosts_test.go`, `doctor_fix_test.go` (normalize, plan, backup,
  idempotency). Full `go test ./...` + `go vet ./...` green.

## Shipped in the second pass (2026-07-15, post-audit)
- **Agent registration repair** — key present + `agent.id` empty → Register (same call
  as the doctor check; mints UUID, persists id/name, adopts a server-minted per-machine
  `agentKey` like init does). Network repair, ordered LAST so an unreachable server
  can't block the offline config fixes. `internal/cmd/doctor_repair_agent.go`.
- **Config perms** — `config.toml` mode & 0o077 → chmod 0600 (POSIX only).
- **Corrupt TOML** — backed up then regenerated from defaults; ALWAYS individually
  confirmed (a bare `--yes` never consents); declining aborts the whole run so nothing
  is ever saved over a file the user didn't agree to replace.
- **API key validity classification** — `classifyAuthError`: Register 401/403 → "API
  key rejected — run `unarr login`"; distinct from the "no key" check.
- **Repair loop hardening** — continue-on-error; everything that succeeded is saved,
  failures are listed and returned as the exit error.
- Audit fixes: `isUnarrBrandHost` now catches scheme-less/`unarr.app.` entries;
  `normalizeAPIURL` preserves userinfo + lowercases scheme; doctor's "API reachable"
  prints the discovery host actually probed; `resolvedConfigPath()` reused everywhere.

## Still TODO (guidance-only today, not auto-applied)
Re-auth (`unarr login` flow), stream-port rebind (needs daemon-running detection to
avoid false positives), ffmpeg/ffprobe presence check (ResolveFFmpeg auto-downloads
~50MB as a last resort — needs a lookup-only variant first), HW-accel fallback,
service (systemd/launchd) install, `unarr update`, clock-skew warning. The checks
surface these with the exact command to run; wiring them into `--fix` is the next
increment.
Trigger for this plan: Discord user on 1.3.7-beta got `torrentclaw: Not found — the
requested resource does not exist (HTTP 404): {"error":"Not found"}` on `unarr stats`
and `unarr search`, while `unarr version` worked.

## Root cause of the trigger (found, must inform the design)

Chain:

1. Config `Default()` sets `auth.api_url = https://unarr.app`, `auth.mirrors =
   [https://torrentclaw.to, https://torrentclaw.com]` (`internal/config/config.go`).
2. `unarr search` / `unarr stats` go through the `torrentclaw/go-client` →
   `GET {api_url}/api/v1/search` and `/api/v1/stats`.
3. `unarr.app` is the **clean brand** deployment. Its middleware brand-gate
   (`src/middleware.ts` → `isUnarrAllowedPath`, `src/lib/branding/routes.ts`) allows
   only a tight allow-list of `/api/v1/*` (`mirrors`, `debrid`, `stream`). Everything
   else → `{"error":"Not found"}` **404 by design** ("ZERO TorrentClaw surface").
   `/api/v1/search` and `/api/v1/stats` are **not** on the list.
4. The mirror transport (`internal/agent/mirror_transport.go` + `IsTransient` in
   `mirror_pool.go`) only rotates to `torrentclaw.to/.com` on **transient** errors
   (502/503/504/408 + connection-level). A **404 is not transient** → no failover →
   the user sees the brand-block 404 even though the mirrors serve the endpoint.
5. `unarr doctor` "API reachable" probes only `GET /api/health`, which is **200 on
   unarr.app** → doctor is **falsely green** and never surfaces the real problem.

Verified live (2026-07-15):

| Host | `/api/health` | `/api/v1/stats` | `/api/v1/search` |
|---|---|---|---|
| unarr.app | 200 | **404 `{"error":"Not found"}`** | **404 `{"error":"Not found"}`** |
| torrentclaw.com | 200 | 200 | 200 |
| torrentclaw.to | 200 | 200 | 200 |

### Two separate fixes come out of this

- **Product fix (source of the bug, do this regardless of doctor):** discovery calls
  (`search`, `stats`, `popular`, `autocomplete`, `trending`, `upcoming`) must reach a
  TorrentClaw host, never the unarr.app primary. Recommended: the discovery
  (`go-client`) client resolves its base to the first **TorrentClaw** entry of
  `{api_url} + mirrors` (unarr.app is agent/stream/debrid only), while the agent
  client keeps using `api_url`. Alternative (weaker): let the mirror transport treat a
  brand-block 404 on the unarr primary as rotate-able — risky, 404 is normally
  definitive. **Do not** expose discovery on unarr.app (violates the brand rule).
- **Safety net (this plan):** `unarr doctor --fix` detects this class ("primary serves
  health but 404s discovery") and repairs it, plus the long tail of other typical
  misconfigurations.

## Design of `unarr doctor --fix`

### Flags
- `--fix` — attempt to repair every failed/warned check that has a known remedy.
- `--yes` / `-y` — non-interactive; auto-apply only **safe** repairs, skip anything
  needing a secret or a human choice.
- `--dry-run` — print what would change, change nothing.
- default (no flag) — diagnostics only (today's behaviour, unchanged).

### Framework refactor (reuse, don't fork doctor)
Turn each check into a struct instead of the current inline closures:

```go
type Check struct {
    Name        string
    Group       string
    Run         func(cfg *config.Config) (status, msg string)   // pass|warn|fail
    Fix         func(cfg *config.Config) (changed bool, err error) // nil = not auto-fixable
    Safe        bool // true → allowed under --yes without a prompt
}
```

`--fix` loop per check: `Run` → if pass, next → if fail/warn and `Fix != nil`:
confirm (unless `--yes && Safe`) → `Fix` → **re-Run to confirm the repair** → record
fixed/failed/skipped. Persist the config once at the end (single `config.Save`) after a
**timestamped backup** `config.toml.bak.<unix>`. Final summary + non-zero exit if any
FAIL remains.

### Safety rules for `--fix`
- Back up `config.toml` before the first write; never lose the user's file.
- Interactive confirm by default; `--yes` auto-applies **only** `Safe` fixes.
- Never print/log the API key; never send it to a non-configured host.
- Idempotent: re-running on a healthy install changes nothing.
- Every branch logs why (no silent no-ops).

## Checks and their repairs

### A. Config integrity
1. **Config file missing** → create from `Default()` (extract the writer from
   `init.go`). Safe.
2. **Config unreadable / invalid TOML** → back up to `.bak`, offer to regenerate
   defaults (interactive; destructive-ish → not `--yes`).
3. **Deprecated / renamed keys** → run the existing `migrate.go` migration path. Safe.
4. **Config perms too open** (holds the API key) → `chmod 600`. Safe.

### B. Auth & connectivity (the important group)
5. **`api_url` malformed** — missing scheme, trailing `/`, or an embedded path
   (`.../api`, `.../api/v1`) → normalize: force `https://`, strip trailing slash,
   strip any `/api...` suffix. Safe.
6. **API key missing** → run the `unarr login` browser/device flow (reuse
   `browserAuth` from `login.go`), persist key. Interactive.
7. **API key invalid/expired** (register or an authed probe returns 401/403) →
   re-auth via `unarr login`. Interactive.
8. **Primary `api_url` unreachable** (health connection error) → probe each mirror;
   if one is healthy, offer to promote it to primary (or confirm failover covers it).
   Interactive (changes routing).
9. **★ Discovery 404 on primary (the trigger bug)** — probe a cheap discovery
   endpoint (`GET /api/v1/stats` or `/api/v1/search?q=test&limit=1`) against the
   **effective** discovery client. If it 404s **and** a TorrentClaw mirror returns 200
   → repair by (a) `unarr mirrors update` to refresh the brand-aware list, and (b)
   applying the product fix's routing (discovery → first TC host). If the product fix
   already routes discovery to a TC host this check just goes green. Safe once the
   product fix lands; until then, interactive (it changes routing).
10. **Empty / stale mirror list** → `unarr mirrors update` (pull `/api/v1/mirrors`).
    Safe.
11. **Agent not registered** (`agent.id` empty) → register (reuse doctor's `Register`
    call), persist `agent_id`. Safe.
12. **Clock skew** — compare server `Date` header vs local clock; if off by more than
    a couple minutes, **warn** (causes TLS/session failures). Not auto-fixable — flag
    only.

### C. Downloads & storage
13. **Download dir not configured** → set to the platform default
    (`~/Downloads/unarr`, XDG-aware). Safe.
14. **Download dir does not exist** → `mkdir -p`. Safe.
15. **Download dir not writable** → attempt to fix perms; else warn with the OS error.
16. **Low disk space** → warn (not fixable).
17. **Stream port(s) in use / privileged** (`stream_port` 11818 / https 11819) →
    detect bind failure; offer to pick a free high port and persist. Interactive.

### D. Runtime dependencies
18. **ffmpeg/ffprobe missing** while transcode enabled → offer to fetch the bundled
    ffmpeg / print the install path. Interactive.
19. **HW accel configured but unavailable** (`hwaccel.go` probe fails) → fall back to
    `hwaccel = auto` (or software) and warn. Safe.
20. **Service not installed / not running** (systemd/daemon installs) → offer to
    (re)install and start the service. Interactive.
21. **Version outdated** → offer `unarr update` (respect the signed-update path).
    Interactive.

## Reuse map (no duplication — HARD RULE)
- Fresh-config writer ← `init.go`
- Login / key persist ← `login.go` (`browserAuth`)
- Key/schema migration ← `migrate.go`
- Interactive config edit ← `config_menu.go`
- Mirror refresh ← `mirrors` command / `/api/v1/mirrors`
- Health / Register probes ← existing `doctor.go` closures (moved into `Check.Run`)
- Mirror pool / transient policy ← `internal/agent/mirror_pool.go`

## Test plan
- Table-driven unit tests per `Check.Fix` (before/after config, idempotency: second
  run is a no-op).
- Golden test for the trigger case: `api_url = unarr.app` + healthy mirrors → check #9
  detects and repairs → `unarr search` works after `--fix`.
- `--dry-run` writes nothing; `--yes` skips unsafe fixes; broken-TOML path backs up and
  never data-loses.
- Cross-platform download-dir defaults (linux/darwin/windows).

## Rollout
1. Land the **product fix** (discovery → TorrentClaw host) first — it fixes every
   already-installed 1.3.x agent without them running anything.
2. Ship `doctor --fix` as the durable safety net + the interactive repairs for the
   long tail.
3. Bump `doctor` "API reachable" to also probe one discovery endpoint so the plain
   `unarr doctor` stops being falsely green.
4. CLI release (signed) — **requires explicit per-turn permission** before tagging.
