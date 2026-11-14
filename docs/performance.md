# Performance

What one probe pass costs, how it was measured, and what CI gates. The
measurement code lives in `internal/perf`: benchmarks, a profiling test and a
soak, all hermetic — synthetic documents and configs generated in code, fake
seams, no host, no daemon, no network.

## How to reproduce

```sh
go test -run '^$' -bench . -benchmem ./internal/perf      # the benchmarks
go test -run TestProfileFullPass -v ./internal/perf       # CPU and heap pprof
go test -run TestSoakLoop -v ./internal/perf              # the soak numbers
go test -run TestAllocationsPerCheck ./internal/perf      # the allocation gate
```

Set `SIMPLEX_PROFILE_DIR=/some/dir` to keep the profiles from the profiling
test; by default they live in a temporary directory and disappear with it.

## What one pass costs

Medians of five runs of forty iterations on the CI runner (ubuntu, 4 vCPU),
`internal/perf`, Go 1.27:

| Benchmark | 10 checks | 100 checks | 1,000 checks | Budget at 1,000 |
| --- | --- | --- | --- | --- |
| `severity.Evaluate` | 3.5 µs | 45 µs | 443 µs | **886 µs** |
| `document.Encode` | 24 µs | 251 µs | 2.16 ms | **4.33 ms** |
| `metrics.Write` (25k series at 1,000) | 103 µs | 856 µs | 8.42 ms | **16.8 ms** |
| `table.Render` | 5.2 µs | 66 µs | 415 µs | **830 µs** |
| `check.Run` (fake mesh) | 214 µs | 1.36 ms | 13.0 ms | **26.1 ms** |
| `config.Parse` | 69 µs | 546 µs | 5.07 ms | **10.1 ms** |
| `overlay.Parse` | 29 µs | 256 µs | 2.22 ms | **4.45 ms** |

Budgets are **twice the measurement**, per the S5 decision. They are recorded,
not enforced: shared CI runners are too noisy to fail a change on time. The
gates are the deterministic ones below.

A scrape of a 1,000-check document is 8.4 ms of encoding and a few kilobytes
written; `serve`'s read and write timeouts are 5 s and 10 s, so a scrape has
two orders of magnitude of headroom at the largest shape tested.

## Allocations

Allocations are deterministic for these paths, so they are gated rather than
watched. Measured on CI, per operation and as a ratio per check:

| Shape | Engine | Per check | Full pass | Per check |
| --- | --- | --- | --- | --- |
| 10 checks | 31 | 3.1 | 1,844 | 184 |
| 100 checks | 225 | 2.3 | 15,997 | 160 |
| 1,000 checks | 2,038 | **2.0** | 158,087 | **158** |

Flat as the mesh grows, which is the property that matters: host derivation
and the pair rule do not allocate per host or per pair beyond the document
they build. `TestAllocationsPerCheck` fails at twice these figures (62 / 450 /
4,076 for the engine, 3,688 / 32,006 / 316,184 for a full pass).

## The soak

`TestSoakLoop` runs the real loop for 3,000 passes in a synthetic bubble,
publishing a fresh document every pass, and samples the settled heap (`GC`
then `ReadMemStats`) and the goroutine count every 100 passes.

Measured: heap 388.3 KiB after warmup, 395.5 KiB at the end — **+7.2 KiB over
2,700 passes**, no slope — and goroutines flat at 4. The gate is twice the
measured growth (16 KiB) plus goroutines back to their baseline when the loop
returns. The suspects it was written for are the snapshot store, the HTTP
transport's idle connections and retry timers; none of them grows here.

## A load run

`TestServeUnderFiftyScrapers` starts `serve` in process and hits `/metrics`
with 50 concurrent scrapers under `-race`: 1,000 scrapes in 75 ms (about
13,000/s) on an M4 laptop, every response complete, then a clean exit 0. It
skips in `-short`.

## What the profiles say

A full pass over a fake mesh of 500 hosts costs 4.18 ms on an M4 laptop
(265 passes in a one-second CPU profile). The stages, cumulative:

| Stage | Share of the pass |
| --- | --- |
| `check.prepare` (config + overlay load) | **22.7%** |
| `config.LoadWithClock` | 13.6% |
| `overlay.Load` | 9.1% |
| everything else, per stage | under 9% each |

The fixed cost of a pass is re-reading the config and the overlay, not
probing. `serve` re-reads both every pass on purpose, so an edit takes effect
without a signal; that is a product decision, and at 500 hosts it is a fifth of
the pass. If a future slice caches the loaded files, this is the table that
motivates it.

The heap profile after the pass holds two things: runtime structures (~1.5 MiB)
and the package-level regular expressions compiled once at `check.init`
(~513 KiB). Nothing per pass is retained.

## What is not gated, and why

- **Wall-clock time.** Two runs of one commit on a shared runner differ by more
  than any change worth rejecting. `bench.yml` posts the comparison on a pull
  request instead, and the numbers above are the reference to judge it against.
- **Memory per operation.** Bytes per operation move with the allocator and the
  platform; the deterministic part is the allocation count, which is gated.
