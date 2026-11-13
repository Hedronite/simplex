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
Simplex (probe) ──simplex.probe/v0 + simplex_*──▶ Vigil (query surface) ──▶ Facet (client)
```

Simplex computes per-check `result` and `severity` and the `overall` rollup.
Vigil re-exposes it; it does not invent a second one. Alert-state rules (watch
severities derived from Prometheus/Alertmanager alerts) live in a separate
classifier, not here: Simplex reports only what it can observe itself.

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
one finding per line, exits 0 with warnings and 3 with errors. A clean config
is quiet:

```console
$ simplex validate --config example.yaml
$ echo $?
0
```

`version` prints one line, or a JSON object with `--json`:

```console
$ simplex version
simplex dev
$ simplex version --json
{"version":"dev","commit":"none","built":"unknown"}
```

- `serve` re-runs the probe on an interval and exposes `/metrics` on loopback.
- Releases are produced with reproducible builds; until then, build from source
  with Go 1.27 or newer.

## The document

A run produces one `simplex.probe/v0` document (see `docs/semantics.md` §9 for
the full contract):

```json
{
  "apiVersion": "simplex.probe/v0",
  "generated_at": "2026-11-13T18:00:00Z",
  "vantage": {"id": "lathe", "status": "ok"},
  "overall": "YELLOW",
  "hosts": [
    {"id": "box", "severity": "YELLOW",
     "reasons": ["http_unexpected_status", "prom_pair_degraded", "unexpected_dark"]},
    {"id": "lathe", "severity": "GREEN", "reasons": []}
  ],
  "checks": [
    {"host": "box", "kind": "prom_up", "target": ":9090/metrics", "result": "down",
     "severity": "YELLOW", "expected": "light",
     "reasons": ["http_unexpected_status", "prom_pair_degraded", "unexpected_dark"]},
    {"host": "lathe", "kind": "prom_up", "target": ":9090/-/ready", "result": "up",
     "severity": "GREEN", "expected": "light", "reasons": []}
  ]
}
```

Field names and the 16 reason codes are frozen at S0 (see §4 and §13 of
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
     until: 2026-12-01T00:00:00Z}
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

## Metrics

`serve` exports gauges with the `simplex_` prefix. Labels carry host slugs and
check kinds only — never addresses. The listener binds loopback by default;
the provisional address is `127.0.0.1:10081` (unconfirmed; S2 finalizes it —
do not scrape it in production config yet). The document writer of record is
`serve --out`, which writes the same snapshot it serves on every interval;
`check --json --out` on a timer is the supported fallback.

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
