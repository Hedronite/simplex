# Simplex metrics

`simplex serve` probes on an interval and serves the latest result as
Prometheus text (version 0.0.4) on `GET /metrics`, loopback by default
(`127.0.0.1:10081`; the address is the `contract.MetricsListen` constant).
`/metrics` and `serve --out` render from the same immutable snapshot, so a
scrape never sees a half-updated document.

## Scope

Simplex reports what it probes. There are **no disk series** — disk severity
reaches the mesh summary through Cubit, not here — and no alert-state series:
alert-derived judgments belong to the alert classifier. Labels carry host
slugs, check kinds and target forms only; **no label ever holds an address**,
and a tracked-file scrub plus a label scrub test keep it that way.

## Series

| Series | Labels | Meaning |
| --- | --- | --- |
| `simplex_build_info` | `version`, `commit` | constant 1 |
| `simplex_overall_severity` | none | worst severity of the last completed run: 0 GREEN, 1 YELLOW, 2 RED |
| `simplex_vantage_status` | `status` | one-hot over `ok`, `blind`, `unchecked` |
| `simplex_host_severity` | `host` | same scale, one series per host |
| `simplex_check_severity` | `host`, `kind`, `target` | same scale, one series per check |
| `simplex_check_result` | `host`, `kind`, `target`, `result` | one-hot over `up`, `down`, `unknown` |
| `simplex_check_reason` | `host`, `kind`, `target`, `reason` | one-hot over all 16 frozen reason codes for **every** check: 1 when the check carries the code, 0 otherwise |
| `simplex_last_run_timestamp_seconds` | none | end of the last completed run, Unix seconds |
| `simplex_document_age_seconds` | none | now minus the snapshot's `generated_at`, computed at scrape |
| `simplex_run_duration_seconds` | none | duration of the last completed run |
| `simplex_run_errors_total` | none | counter of runs that produced no document |

Rulings baked into this table:

- `host` is a slug; `target` is the port-and-path form from
  `docs/semantics.md` section 2, never an address.
- `simplex_check_result` and `simplex_check_reason` are one-hot over the
  **frozen enums**, not over what the document carries: every check exports a
  series for every result value and every reason code, 1 or 0. Series stay
  stable, so a clearing reason reads 1 to 0 and PromQL needs no `absent()`;
  an unknown code can never become a label.
- `simplex_check_reason` is driven by the frozen enum even if a document
  carried a code the contract does not know: the label space cannot grow past
  the sixteen codes.

## Cardinality

Per snapshot:

- 1 build series.
- 1 overall series.
- 3 vantage one-hot series.
- 1 series per host.
- 20 series per check: 1 severity + 3 result one-hot + 16 reason one-hot.
- plus `simplex_last_run_timestamp_seconds`, `simplex_document_age_seconds`,
  `simplex_run_duration_seconds` and `simplex_run_errors_total`.

The per-check block is the 20 the formula promises; a run with three hosts and
five checks exports 1 + 1 + 3 + 3 + 100 + 4 = 112 series.

## Cold start

Before the first run completes there is no snapshot. `/metrics` serves only
`simplex_build_info` and `simplex_run_errors_total` (0). No `go_` or
`process_` runtime series are exported at any time — the hand-rolled encoder
knows exactly the families above. A scraper that starts before the first pass
sees the build line, the zero counter, and nothing else; after the first pass
the full set appears at once, never half-updated.

## Operational shape

- One route: `GET /metrics`. Everything else is 404, other methods 405.
- The listener is loopback by default; a non-loopback `--listen` is refused
  unless `--unsafe-allow-non-loopback` is set, and then a warning is logged on
  every start: `/metrics` has no authentication.
- A failing pass after startup is counted in `simplex_run_errors_total` and
  logged by field path only — never an address, an overlay value or dialer
  error text — and the last good snapshot keeps serving.
- `serve` never exits on severity; SIGINT and SIGTERM finish the pass in
  flight and exit 0.

Seating `serve` on a host and adding its scrape job to a Prometheus
configuration is a separate deployment decision, not part of this repository's
build.
