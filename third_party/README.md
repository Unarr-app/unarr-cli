# third_party/torrent

Copy of `github.com/anacrolix/torrent` **v1.61.0** (MPL-2.0, see `torrent/LICENSE`), wired in
with `replace github.com/anacrolix/torrent => ./third_party/torrent` in the root `go.mod`.
Only the non-test sources of the packages the CLI imports (linux/windows/darwin) are kept.

## Differences from upstream v1.61.0

1. `client-tracker-announcer.go`: the interval a tracker returns is floored at
   `minTrackerAnnounceInterval` (1 min). Upstream schedules the next announce at
   `Completed + Interval` with no minimum, so a tracker answering `interval=0` (or a negative
   UDP interval) makes the dispatcher announce in a hot loop (~2400/s, one goroutine each).
   Regression test: `internal/engine/tracker_interval_test.go`.
2. `config.go`, `storage/iter.go`: `gofmt -s` only (`make fmt` runs `gofmt -s -w .`).

## Dropping it

Remove the `replace` line and this directory once upstream floors the announce interval
(master as of 2026-10-24 still does not: `nextAnnounceEvent` returns `Completed+Interval`).
Dependabot cannot bump a replaced module: bump the copy by hand.
