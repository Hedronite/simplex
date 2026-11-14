# The Simplex dashboard

Simplex ships one Grafana dashboard as provisioning config for an existing
Grafana — the dashboard JSON (`grafana/dashboards/simplex.json`) and the file
provider that loads it (`grafana/provisioning/dashboards/simplex.yaml`).
Nothing here installs anything on a Grafana host: the files go through the
normal deployment decision.

## What it needs

- Grafana 13.x (the JSON carries `schemaVersion` 42; Grafana migrates older
  numbers up on import).
- A Prometheus datasource that scrapes `simplex serve`, with the uid
  `lea-prom`: every panel and every variable in the dashboard names exactly
  that datasource by uid. The uid must match an existing datasource in your
  Grafana; if yours differs, change it once in the JSON.

## Provisioning

1. Copy `grafana/dashboards/simplex.json` into
   `/var/lib/grafana/dashboards/simplex/` (create the directory).
2. Copy `grafana/provisioning/dashboards/simplex.yaml` into your Grafana
   provisioning directory (`/etc/grafana/provisioning/dashboards/` by
   default). The provider scans the `simplex` subdirectory only, so it does
   not collide with another provider already scanning the parent directory.
3. Restart Grafana (or wait out the provider's update interval). The
   dashboard appears under the `Simplex` folder.

The dashboard uid is fixed (`simplex`), which makes re-provisioning idempotent:
a newer file replaces the older one rather than duplicating it.

## Panels, top to bottom

| Panel | Reads | How to read it |
| --- | --- | --- |
| Overall | `simplex_overall_severity` | The worst severity of the last completed run, mapped 0 GREEN, 1 YELLOW, 2 RED. Held and expected-dark checks never raise it. |
| Vantage | `simplex_vantage_status == 1` | Which vantage state is active. Blind reads as **cut off (blind)**, not as a dead mesh: the vantage cannot see the tailnet, which says nothing about the mesh itself. |
| Document freshness | `simplex_document_age_seconds`, with `simplex_last_run_timestamp_seconds` beside it | The age is the freshness signal; the wall-clock time answers "since when". Thresholds are multiples of the run interval — 3 × interval (yellow) and 20 × interval (red) at the defaults. An aging document means a wedged probe loop or a stalled `serve`. |
| Hosts | `simplex_host_severity` | Per-host severity over time. |
| Checks | `simplex_check_severity` joined with the result and reason one-hots (`== 1`) by label | One row per check instance (host, kind, target). The severity column carries the palette; a check with two carried reasons shows two rows. |
| Run health | `simplex_run_duration_seconds`, `increase(simplex_run_errors_total[$__rate_interval])` | How long a pass takes and how often one produces no document. A failing pass after startup is counted here, not fatal. |
| Build | `simplex_build_info` | Which build of the binary the data is from. |

Variables `host` and `kind` filter the host and check panels; both come from
the exported label values.

## Blind and stale

- **Blind** (`vantage status blind`, exit 5): every check reads `unknown`,
  every severity RED, and the Vantage panel shows **cut off (blind)**. The
  mesh may be perfectly healthy; only this vantage cannot see it. Treat a
  blind run as "go look at the vantage", not as a page.
- **Stale** (document age climbing): the last snapshot is old. Either `serve`
  has stopped, or every recent pass failed to produce a document — check Run
  health for a rising error counter, and the `serve` journal for the failing
  field path. A stale snapshot keeps its last values; it never shows a
  current GREEN from a dead loop as live.

The series, labels and cardinality behind every panel are in
`docs/metrics.md`; the query contract is `docs/semantics.md`.
