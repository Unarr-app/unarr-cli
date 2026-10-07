# Remote library performance and verification

Measured on 2026-09-19, Linux Mint 22.3, kernel 6.8.0-139, AMD Ryzen 7 7700X
(8 cores / 16 threads), Go 1.26.0, default GOMAXPROCS=16. Benchmarks used
`-benchtime=1s -count=3 -benchmem`; figures below are medians of three runs.
Compilation, race tests and other validation were not run alongside benchmarks.

These are reproducible local implementation benchmarks, **not a Zurg comparison**.
They measure the filesystem/HTTP/NNTP data path. After centralizing account
management on the web, those paths remain the same; initial web catalog fetches
and web-side URL resolution are additional control requests not measured here.
No real provider credentials or equivalent Zurg installation were available for
an end-to-end comparison. WAN throughput, API quotas, article retention, cold
indexing and provider failures can dominate real workloads. There is no evidence
here that this implementation is universally faster than Zurg.

## Results

| Operation | Median | Allocated bytes/op | Allocations/op |
| --- | ---: | ---: | ---: |
| Serial stat, 1,000 files | 25.56 ns | 0 | 0 |
| Serial stat, 100,000 files | 25.88 ns | 0 | 0 |
| Open/stat/close, 100,000 files | 48.27 ns | 64 | 1 |
| Build snapshot, 1,000 files | 1.015 ms | 536,944 | 6,031 |
| Build snapshot, 100,000 files | 251.27 ms | 55,114,176 | 600,552 |
| Repeated release PROPFIND, cache disabled | 16.734 µs | 22,106 | 130 |
| Repeated release PROPFIND, cache enabled | 1.516 µs | 3,294 | 23 |
| HTTP range, 4 KiB | 52.203 µs | 11,092 | 96 |
| HTTP range, 1 MiB | 376.795 µs | ~40,054 | 96 |
| HTTP range, 8 MiB | 2.872 ms | ~40,000 | 96 |
| Usenet cached tail read, 4 KiB | 5.492 µs | 15,656 | 38 |

The paired PROPFIND benchmark uses the same current implementation and request,
with only the listing cache toggled. Warm responses are **11.0× faster**, with
**85.1% fewer allocated bytes**. Uncached timings ranged from 14.436–20.376 µs;
cached timings ranged from 1.513–1.536 µs. These measurements call the handler
in-process, excluding TCP and rclone. They measure one release directory, not
serialization of the entire 100,000-file tree. Cached responses are keyed by
snapshot generation, path, depth and requested properties, so updates invalidate
listings without waiting for a TTL.

Before preallocating the path index and removing unnecessary ETag formatting
allocations, building the same 100,000-file tree took 294.21 ms and allocated
94,108,308 bytes in 1,501,102 allocations. The final implementation reduces time
by **14.6%**, allocated bytes by **41.4%**, and allocation count by **60.0%**.
Allocated bytes are cumulative allocations during a rebuild, not retained heap
or peak process RSS. Old snapshots remain alive while readers reference them.

Parallel random stat across 100,000 paths measured 4.652 ns/op with no allocations.
That figure is aggregate throughput divided by operation count across 16 workers,
**not individual request latency**; use the serial measurements for that purpose.

HTTP tests use an in-memory local HTTP origin and include transport and range
validation. Large reads achieved about 2.8–2.9 GB/s. The roughly constant 40 KiB
allocation cost from 1 MiB to 8 MiB reflects streaming rather than buffering the
whole requested range. This does not predict Internet download speed.

The Usenet test uses a 4 MiB synthetic file in 64 KiB articles, with its plan and
tail article already warm. Open/seek/read/close fetches **zero additional NNTP
articles per operation**. Its allocation count includes the test's `io.ReadAll`;
cold discovery and fetching are excluded.

## Resource and operational bounds

- XML listing cache: at most 16 MiB of response buffer capacity and 256 entries;
  individual responses above 1 MiB are streamed without retention.
- GET concurrency: 32; metadata does not consume a media connection. HTTP media
  reads now cancel a response after 30 seconds without upstream read progress;
  DAV writes/flushes have a separate 30-second socket write bound. These are
  inactivity bounds, so a healthy long stream has no total-file timeout.
- Usenet warm plans: 16; media articles use the existing shared bounded cache.
  Each NNTP body has a 16 MiB encoded wire ceiling, including CRLF, dot stuffing
  and terminator, enforced before appending every fragment. This compatibility
  ceiling matches the existing stream reservation assumption and is independent
  of NZB claims. Receive-array capacity is bounded per active connection;
  transient growth copies, decoder allocations and retained cache coexist, so
  this is not a global process RSS bound. Oversized/incomplete/cancelled bodies
  retire their socket; a live caller can reacquire the vacant slot.
- Provider work now runs on the web backend through the existing provider
  clients and concurrency controls. The mount endpoints permit 240 requests per
  user/minute and propagate provider Retry-After. Unchanged release metadata is
  reused; catalog publication is skipped when records are unchanged.
- Mount provider work is spaced by 600 ms per account revision in each backend
  process, with a maximum five-second queue and upstream cooldowns. This bounds
  background scans; it is not a global quota across server replicas or other apps.
- Built-in rclone mount: 4 MiB read buffer per file; no full-file disk cache.

## Reproduction

Final validation passed: `go test -race ./...`, the separate E2E suite with the
race detector, the real Linux FUSE mount test, correctness lint (zero issues),
and architecture gates for Linux, Windows and macOS (zero issues). The CLI also
cross-compiled successfully for Windows/amd64 and macOS/arm64.

The web integration additionally passed 16 dedicated account/reference/route/pacing
tests and the complete web unit suite (13,262 passed, 10 skipped, 5 todo),
TypeScript checking, scoped ESLint and the architectural ratchet. All three
routes were checked against the local Next.js server and returned 401 without
agent authentication. Account protocols use synthetic provider responses;
real account/CDN end-to-end testing still requires a deployed matching backend.

```sh
go test ./internal/remotefs -run '^$' \
  -bench 'Benchmark(Metadata|PropfindCache|SnapshotBuild|HTTPRange|NZBHotTail)$' \
  -benchmem -benchtime=1s -count=3

go test -race ./...
go test -tags e2e -race -count=1 ./test/e2e/...
golangci-lint run --allow-parallel-runners ./...
make arch ARCH_BASE=main

# Requires rclone on PATH and working /dev/fuse + fusermount3:
UNARR_TEST_RCLONE_MOUNT=1 go test ./internal/cmd \
  -run TestRcloneKernelMount -v -count=1
```

The actual Linux kernel mount test used rclone v1.75.1-DEV (installed from the
v1.75.1 Go module). It verifies directory listing, names with spaces and XML
characters, byte-exact reads at the beginning, a 1 MiB offset, the tail and a
backward seek, write rejection and clean unmount. It uses a local synthetic
origin; the web account integration and Usenet sources have separate protocol tests.

Automated coverage includes default-off behavior, configuration validation,
credential redaction, authenticated read-only WebDAV, offline metadata access,
concurrent snapshot refresh, persistence and partial-refresh failures, provider
pagination, account-bound references, shared link renewal, invalid/truncated HTTP ranges, real
NNTP framing with synthetic yEnc articles, direct multi-file NZBs and supported
RAR streaming. See [the platform test matrix](REMOTE_MOUNT_TEST_MATRIX.md) for
subsequent native setup checks, including macOS installation/upgrade and its
remaining system-extension approval requirement.

On 2026-10-07, regression fixes were verified against the integrated main baseline
with synthetic loopback HTTP/NNTP and temporary catalogs. They cover command
validation, receive ceilings, progress/cancellation, strict framing, partial
revision recovery and stable collision names. See the [protocol fixes report](docs/reviews/remote-mount-protocol-fixes-2026-10-07.md)
for actual failing probes and subsequent validation. The September performance
figures above were not remeasured for these changes.

On 2026-09-20, the Windows 11 VM passed the real kernel mount test after automatic
installation of checksum-verified WinFsp 2.1.25156 and rclone 1.75.1. The test
verified special-character paths, byte-exact offset reads, write rejection and
unmount. Setup approval was explicitly supplied by the disposable VM test; its
administrator session did not exercise a non-elevated user's UAC interaction.
Dependency setup also passed cancellation/refusal, checksum/archive validation,
cached reuse and a real official Linux rclone download test. Setup is outside the
streaming path; the performance figures above have not been remeasured for it.
