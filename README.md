# Simplex

Simplex is a mesh probe: it dials a configurable set of hosts and checks from one
vantage point, judges each observation against the mesh's watch semantics, and
reports one structured document, `simplex.probe/v0`, plus `simplex_*` Prometheus
metrics. It turns the repeated by-hand host-matrix check into one reproducible
run.

- **Inspect-only.** It probes and reports. It never mutates a host, never
  silences or routes alerts, and never reads alert state.
- **One verdict.** Every check gets a `result` (what the probe saw) and a
  `severity` (the judgment); `hosts[].severity` and `overall` are derived from
  checks, never assigned.
- **A consumer's vocabulary.** Query surfaces read the document or the metrics
  from a file; they never run Simplex or re-derive its rollup.

## Position in the family

```
Simplex (probe) ──simplex.probe/v0 + simplex_*──▶ a downstream query surface ──▶ a client
```

Simplex computes per-check `result` and `severity` and the `overall` rollup.
A downstream query surface re-exposes it; it does not invent a second one.
Alert-state rules (watch severities derived from Prometheus/Alertmanager
alerts) live in a separate classifier, not here: Simplex reports only what it
can observe itself.

## Quickstart

A run needs a public config (slugs only) and, for any check that needs an
address, a private overlay file that is never committed. `example.yaml` in this
repository is a complete public config:

```console
$ simplex check --config example.yaml
$ simplex check --config example.yaml --json
$ simplex validate --config example.yaml
$ simplex serve --config example.yaml
```

`check` renders the table by default; `--json` prints the `simplex.probe/v0`
document. Exit codes are identical in both modes. The table from a machine with
no route to the mesh (a blind vantage, exit 5 — not a mesh failure):

```console
$ simplex check --config example.yaml
HOST   CHECK                        EXPECTED  RESULT   SEVERITY  REASONS
box    prom_up :9090/metrics        light     unknown  RED       vantage_blind
lathe  prom_up :9090/-/ready        light     unknown  RED       vantage_blind
tower  kube_apiserver :6443         dark      unknown  RED       vantage_blind
tower  node_exporter :9100/metrics  light     unknown  RED       vantage_blind
tower  ts_reach :9100               light     unknown  RED       vantage_blind
$ echo $?
5
```

`validate` loads and checks the config and overlay without probing; it prints
one finding per line, exits 0 with warnings and 3 with errors. On a clean
checkout there is no private overlay to read, so the addresses are not checked
and validate says so:

```console
$ simplex validate --config example.yaml
warning: no overlay found: addresses not checked
$ echo $?
0
```

With an overlay present (never committed), the same command resolves every
check against it and is quiet when the config is valid.

`version` prints one line, or a JSON object with `--json`:

```console
$ simplex version
simplex dev (commit unknown, built unknown)
$ simplex version --json
{"version":"dev","commit":"unknown","built":"unknown"}
```

A source build prints the fallbacks above; a release build prints its version,
commit and built date in the same shapes.

- `serve` re-runs the probe on an interval and exposes `/metrics` on loopback.
- Releases are produced with reproducible builds; until then, build from source
  with Go 1.27 or newer.

## The document

A run produces one `simplex.probe/v0` document (see `docs/semantics.md` §9 for
the full contract):

Captured documents live in the repository as fixtures:
`testdata/green/document.json` is a complete GREEN run (every check answering,
with the expected-dark `:6443` connect read as GREEN), and the fixture replay
test reproduces every fixture byte for byte through the binary on each commit,
so the documented shape and the shipped bytes cannot drift.

Field names and the reason codes are frozen (16 at S0, five enrichment codes added at S3) (see §4 and §13 of
`docs/semantics.md`); changes after S1 are breaking. The check kinds are an open
enum: adding one is a minor change.

## Exit codes

| Code | Meaning | Document written |
| --- | --- | --- |
| 0 | GREEN | yes |
| 1 | YELLOW | yes |
| 2 | RED | yes |
| 3 | config or usage error | no |
| 4 | runtime or I/O error | no |
| 5 | vantage blind (`overall: RED`) | yes |

No error path exits 0, 1 or 2. Under `--json`, error exits (3 and 4) write one
`{"error":{"code","message"}}` line to stderr. Exit 5 is how automation tells
"I am cut off" from "the mesh is down": the document is still written, with
`vantage.status: blind`.

## Configuration

The public config (`simplex.config/v0`) carries slugs only; addresses live in a
private overlay (`simplex.overlay/v0`) that is never tracked. See
`docs/semantics.md` §8 for the full contract; the shapes:

```yaml
apiVersion: simplex.config/v0
vantage:
  id: lathe
  canaries:                      # each names a configured ts_reach check
    - {host: tower, kind: ts_reach, target: ":9100"}
hosts:
  - id: tower
    checks:
      - {kind: ts_reach, expected: light}
      - {kind: kube_apiserver, expected: dark}
holds:                            # temporary announced darks; all fields required
  - {host: tower, kind: ts_reach, reason: planned maintenance,
     until: 2030-01-01T00:00:00Z}
```

- `expected` (`light` or `dark`) is required on every check; there is no
  per-kind default. An expected-dark check that is not up is GREEN
  (`expected_dark`); an unexpected light is YELLOW (`unexpected_light`).
- `holds` are temporary announced darks with a required RFC 3339 `until`. An
  expired hold is ignored and `validate` warns about it. Held checks still run
  and report their real result, but never count toward YELLOW or RED.
- The overlay is keyed by host slug and supplies `address`, optional
  `lan_address`, and per-(kind, target) overrides for `port` and `path`. A check
  that needs an address with no overlay entry fails closed (exit 3), naming the
  missing slugs. Overlay files (`simplex.overlay.yaml`, `*.overlay.yaml`) are
  gitignored; never commit one.

## Tailscale enrichment

Optional, off by default, Linux unix socket only. A `ts_reach` connect says
whether a port answered; the tailnet daemon on the vantage also knows whether
the peer is online and whether traffic runs direct or through a relay.
Enrichment reads that state and attaches it to the check as extra reason
codes: `ts_peer_online`, `ts_peer_offline`, `ts_peer_unmatched`,
`ts_path_direct` and `ts_path_relayed`.

Turn it on in the config:

```yaml
tailscale:                   # optional, off by default
  localapi: true             # required when the block is present
  socket: /var/run/tailscale/tailscaled.sock   # optional, absolute path only
```

- **Informational only.** Enrichment never changes a `result`, a `severity`,
  the rollup or the exit code. It explains an unexplained timeout; it does not
  judge.
- **Access limits.** One request per run: `GET /localapi/v0/status` with a
  constant path and method, no body, over the configured unix socket. No
  root, no capabilities, no `tailscale` command or any process execution, no
  mutating call, and the socket file is never created or written. The request
  times out in 2 s inside the pass deadline; the response is capped, decoded
  into a minimal structure and reduced to booleans — no payload text reaches
  an error or a log.
- **A warning means** the daemon could not be read — socket missing,
  unreadable, slow, oversize or malformed — once per run, by field path. The
  run then behaves exactly as if enrichment were off.

The full contract is `docs/semantics.md` §15.

## Dashboard

`grafana/dashboards/simplex.json` provisions one dashboard on an existing
Grafana, and `grafana/provisioning/dashboards/simplex.yaml` is the file
provider for it. How to install them, what each panel means, and how blind
and stale read are in `docs/dashboard.md`.

## Performance

Benchmark, profile and soak results, and the gates they feed, are in
`docs/performance.md`.

## Serve and metrics

`simplex serve` probes on an interval and serves the latest result as
Prometheus metrics on `GET /metrics`, loopback by default at
`127.0.0.1:10081` (the `contract.MetricsListen` constant — `serve --help`
quotes it from there). It binds loopback only; a non-loopback `--listen` is
refused unless `--unsafe-allow-non-loopback` is set, and then a warning is
logged on every start: `/metrics` has no authentication.

```console
$ simplex serve --help
Run a probe pass every --interval and serve the latest result as
Prometheus metrics on GET /metrics.

Each pass produces one snapshot; /metrics and --out are built from the same
one. With --out, serve is the writer of record for the document file: the
snapshot is written every interval by temp file, fsync and atomic rename.

serve binds loopback only. A non-loopback --listen is refused unless
--unsafe-allow-non-loopback is set, and then a warning is printed on every start:
/metrics has no authentication.

A failing pass after startup is counted (simplex_run_errors_total) and logged
by field path only, and the last good snapshot stays; serve never exits on
severity. SIGINT or SIGTERM finishes the pass in flight and exits 0.

Exit codes: 0 normal stop, 3 config or usage error at startup, 4 the address
cannot be bound or the server fails.

Usage:
  simplex serve [flags]

Flags:
      --config string               config file (default "simplex.yaml")
      --deadline duration           time one pass may take (at most --interval) (default 15s)
  -h, --help                        help for serve
      --interval duration           time between probe passes (default 30s)
      --listen string               address to serve /metrics on (loopback unless --unsafe-allow-non-loopback) (default "127.0.0.1:10081")
      --out string                  write the document to this file every pass (temp file, fsync, atomic rename)
      --overlay string              overlay file (default: $SIMPLEX_OVERLAY, then simplex.overlay.yaml when present)
      --unsafe-allow-non-loopback   allow a non-loopback --listen; /metrics is unauthenticated
```

The real exposition from `example.yaml`, run from a machine with no route to
the mesh (excerpt; the full output has one severity, three one-hot result and
21 one-hot reason series per check). The help block above is the whole
output of `serve --help`, checksum-matched to the built binary:

```console
$ curl -s http://127.0.0.1:10081/metrics
# HELP simplex_build_info Build information for this binary.
# TYPE simplex_build_info gauge
simplex_build_info{version="dev",commit="unknown"} 1
# HELP simplex_overall_severity Worst severity of the last completed run: 0 GREEN, 1 YELLOW, 2 RED.
# TYPE simplex_overall_severity gauge
simplex_overall_severity 2
# HELP simplex_vantage_status One-hot vantage status of the last completed run.
# TYPE simplex_vantage_status gauge
simplex_vantage_status{status="ok"} 0
simplex_vantage_status{status="blind"} 1
simplex_vantage_status{status="unchecked"} 0
# HELP simplex_host_severity Severity of one host: 0 GREEN, 1 YELLOW, 2 RED.
# TYPE simplex_host_severity gauge
simplex_host_severity{host="box"} 2
simplex_host_severity{host="lathe"} 2
simplex_host_severity{host="tower"} 2
# HELP simplex_check_severity Severity of one check instance: 0 GREEN, 1 YELLOW, 2 RED.
# TYPE simplex_check_severity gauge
simplex_check_severity{host="box",kind="prom_up",target=":9090/metrics"} 2
...
```

The full series table, the one-hot rulings, the cold-start behavior and the
cardinality formula are in `docs/metrics.md`.

**Scope:** Simplex reports what it probes. There are no disk series — disk
severity reaches the mesh summary through a companion collector — and no
alert-state series. Labels carry host slugs, check kinds and target forms
only, never addresses.

**Deployment is a separate decision:** building `serve` is part of this
repository; seating it on a mesh host and adding its scrape job to a
Prometheus configuration needs its own sign-off.

## Security notes

- Loopback-only listeners by default; a wider bind requires an explicit
  unsafe flag.
- Secrets and addresses come from the private overlay, `--overlay`, or
  `SIMPLEX_OVERLAY` — never from argv values or the public tree.
- Port 18080 is never dialed; config load rejects any check that names it.
- `prom_up` probes HTTP only, on a fixed path allowlist
  (`/metrics`, `/-/ready`, `/-/healthy`), with no redirects, no userinfo,
  no query strings, and a 1 MiB body cap.
- `kube_apiserver` is a TCP connect only: no TLS handshake, no HTTP, no
  credentials.
- The public repository carries host slugs only. CI scrubs every tracked file
  for addresses, hostnames and secrets; the allowlist is fixed in
  `docs/semantics.md` §14.

## Contributing and license

Contributions are welcome under the Apache-2.0 license (see `LICENSE`).
Development and contribution rules — including the Developer Certificate of
Origin sign-off required for every commit — are in `CONTRIBUTING.md`. Please
report security issues privately; see `SECURITY.md`. The prose contract for
everything above lives in `docs/semantics.md`; when code, schema and prose
disagree, a test fails.
