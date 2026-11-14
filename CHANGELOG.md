# Changelog

All notable changes to Simplex are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/); the project does not yet
consider 1.0.0 reached, so breaking changes are called out explicitly.

## [0.1.0]

The first release: the probe, its document and metrics contracts, the CLI,
and the release tooling.

### Added

- The `simplex.probe/v0` document, the `simplex.config/v0` and
  `simplex.overlay/v0` formats, the frozen reason-code enum, and the JSON
  Schemas that pin all three, with conformance tests binding code, schema and
  prose together.
- `simplex check`: one probe pass from a configurable vantage — the human
  table by default, the document with `--json`, `--out` for the document
  file, and the exit-code mapping: 0 GREEN, 1 YELLOW, 2 RED by verdict,
  3 config or usage error, 4 runtime or I/O error, 5 vantage blind.
- `simplex validate`: strict config and overlay loading without probing, with
  every finding on stdout in human or `--json` form.
- `simplex serve`: probe passes on an interval with `GET /metrics` on
  loopback, the `--out` writer of record, the loopback-only bind guard behind
  `--unsafe-allow-non-loopback`, and post-startup failures counted rather
  than fatal.
- Prometheus metrics under the `simplex_` prefix: severity gauges, one-hot
  result and reason families over the frozen enums, the vantage one-hot, and
  the last-run, age, duration and error series.
- Optional Tailscale enrichment: one read-only LocalAPI request per run over
  a Linux unix socket, adding the five `ts_peer_*` and `ts_path_*` reason
  codes to `ts_reach` checks. Informational only; never changes a verdict.
- A Grafana dashboard with its file provider, and the drift test that keeps
  the dashboard's queries in step with the exported series.
- A release pipeline: reproducible archives and binaries, a per-archive
  CycloneDX SBOM, a cosign keyless signature bundle over the checksums file,
  pinned workflow actions, and benchmarks, profiles and a soak pass in CI.
### Security

- Inspect-only by design: probes send no credentials, never mutate a host,
  and never read alert state.
- Addresses live only in the private overlay; the public tree carries slugs
  and is scrubbed in CI.
- Loopback-only listeners by default; port 18080 is never dialed.

[0.1.0]: https://github.com/Hedronite/simplex/releases/tag/v0.1.0
