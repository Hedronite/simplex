# Simplex semantics (S0 contract)

This is the prose contract for `simplex.probe/v0`, `simplex.config/v0` and `simplex.overlay/v0`. The Go vocabulary lives in `internal/contract`; the schemas live in `schema/`. When the three disagree, a test fails. Anything marked **provisional** is not frozen at S0.

## 1. Principles

- Simplex reports what it can observe directly. It never reads alert state, and it never checks disk.
- `check` is stateless. No hold timers, no memory between runs.
- `result` is the observation. `severity` is the judgment. They are separate fields and never substitute for each other.
- `hosts[].severity` and `overall` are **derived** from checks. They are never assigned on their own.
- Public config, fixtures, docs and labels carry host slugs only. Addresses live in the private overlay.

## 2. Check kinds

The enum is open: adding a kind is a minor change. A check instance is identified by (host slug, kind, target). `target` is an opaque string in the document; its form is fixed per kind below and never contains an address.

| Kind | Probe | Default port | Target form |
| --- | --- | --- | --- |
| `ts_reach` | TCP connect over the tailnet | 9100 | `:<port>` |
| `node_exporter` | `GET /metrics` over the tailnet | 9100 | `:<port>/metrics` |
| `prom_up` | `GET <path>` on a Prometheus | 9090 | `:<port><path>` |
| `kube_apiserver` | TCP connect over the tailnet | 6443 | `:<port>` |
| `lan_reach` (provisional) | TCP connect over the LAN address | 22 | `:<port>` |
| `ssh` (provisional) | TCP connect plus `SSH-` banner | 22 | `:<port>` |

- `expected` (`light` or `dark`) is **required on every check**, in config and in the document. There is no per-kind default.
- Port 18080 is never dialed. Config load rejects any check that names it, and the dialer refuses it too.
- `path` is legal only on `prom_up`, in config and in overlay overrides. A `path` on any other kind is a config error (exit `3`).
- `prom_up` path comes from an allowlist: `/metrics`, `/-/ready`, `/-/healthy`; default `/-/ready`. HTTP only, host must be an overlay slug, no userinfo, no query, no redirects, 1 MiB body cap.
- `kube_apiserver` is a TCP connect to `:6443` and nothing more: no TLS handshake, no HTTP request, no credentials. Another project owns TLS and everything above the connect, so a closed or filtered port is the only thing this kind can report.
- `lan_reach` and `ssh` land in the third S1 slice. Their rows are intent. The kind names are frozen.

## 3. Result classification

`down` is a definitive negative. `unknown` is indeterminate: a timeout, an unreachable network, or a DNS failure. A timeout never serializes as `down`.

| Kind | `up` | `down` | `unknown` |
| --- | --- | --- | --- |
| `ts_reach`, `lan_reach` | connect accepted; connect refused (the host answered, so the path is alive) | none | timeout, unreachable, DNS error |
| `node_exporter` | HTTP 200 | connect refused; HTTP 404 or 5xx | timeout, unreachable, DNS error |
| `kube_apiserver` | connect accepted | connect refused | timeout, unreachable, DNS error |
| `prom_up` | 200; 503 on `/-/ready`; 401 or 403; 200 on `/metrics` without `prometheus_build_info` in the first 64 KiB | connect refused; HTTP 404 or 5xx | timeout, unreachable, DNS error |
| `ssh` (provisional) | banner starts with `SSH-` | connect refused; other banner | timeout, unreachable, DNS error |

Retry: one retry after 250 ms, on timeout only. Never retry a refusal.

## 4. Reason codes

Exactly 16 codes, closed, snake_case, frozen at S0. Add a code is a minor change; remove or rename is breaking. `watchdog_only`, `probe_timeout` and `peer_prometheus_*` are not Simplex codes.

An **outcome** code says what the probe saw. A **verdict** code says why the severity is what it is. Outcome codes are informational: they appear alongside a verdict code and never replace one. A check carries at most one outcome code. A healthy HTTP check (200, no body problem) carries none, and a failure with no transport detail (unreachable, DNS error) carries none. Under a blind vantage a check carries only `vantage_blind`. `checks[].reasons[]` is sorted bytewise ascending and has no duplicates. `hosts[].reasons[]` is not re-sorted: it follows the order in section 6.

| Code | Class | Emitted when |
| --- | --- | --- |
| `tcp_connect_ok` | outcome | TCP connect accepted |
| `tcp_connect_refused` | outcome | TCP connect refused |
| `tcp_connect_timeout` | outcome | TCP connect timed out |
| `http_timeout` | outcome | connected, response timed out |
| `http_unexpected_status` | outcome | HTTP 404 or 5xx |
| `prom_not_ready` | outcome | 503 on `/-/ready` |
| `prom_auth` | outcome | HTTP 401 or 403 |
| `metrics_body_unexpected` | outcome | 200 on `/metrics` without `prometheus_build_info` in the first 64 KiB |
| `expected_dark` | verdict | check is expected dark and is not up |
| `unexpected_light` | verdict | check is expected dark and is up |
| `unexpected_dark` | verdict | check is expected light and is down |
| `held` | verdict | an unexpired hold matches |
| `probe_unknown` | verdict | check is expected light and result is `unknown` |
| `prom_pair_down` | verdict | every non-held `prom_up` check is not up (two or more) |
| `prom_pair_degraded` | verdict | some, not all, non-held `prom_up` checks are not up |
| `vantage_blind` | blind | the vantage cannot see the mesh |

## 5. Severity rules

Evaluated per check, in this order. The first rule that matches decides the severity; outcome codes from the probe are always kept.

1. **Blind vantage:** result `unknown`, severity RED, reasons `[vantage_blind]`. Terminal: it ends evaluation for the check, overrides holds and expected-dark, and the pair pass (rule 5) is skipped under a blind vantage. A blind check carries only `vantage_blind`.
2. **Held** (an unexpired hold matches host and kind): severity GREEN, reasons gain `held`, whatever the result. The check still runs and reports its real result.
3. **Expected dark:** not up gives GREEN `expected_dark`. Up gives YELLOW `unexpected_light`. The prom status map does not apply here.
4. **Expected light:**
   - up: GREEN. The outcome codes `prom_not_ready`, `prom_auth` and `metrics_body_unexpected` raise it to YELLOW; they refine the `up` and add no verdict code.
   - down: YELLOW `unexpected_dark`.
   - unknown: YELLOW `probe_unknown`. Never RED on its own.
5. **Prometheus pair**, after rules 1 to 4 and never under a blind vantage, over the non-held, expected-light `prom_up` checks. With fewer than two such checks the rule is off. If none is up, each of them becomes RED and gains `prom_pair_down`. If at least one is up and at least one is not, each not-up check becomes YELLOW and gains `prom_pair_degraded`. The pair code is added to the check's other reasons, not substituted. Severity lands on `checks[]` and flows up by derivation (section 6).

An unexpected `node_exporter` that is not up is YELLOW, never RED on its own. A held `prom_up` check is outside the pair, so held plus down never fires the pair rule.

### Combination rows

Pinned so the order cannot be guessed. First matching rule of the list above; pair pass not shown.

| Expected | Result | Outcome code | Verdict | Severity |
| --- | --- | --- | --- | --- |
| dark | up | none (200) | `unexpected_light` | YELLOW |
| dark | up | `prom_not_ready` (503 on `/-/ready` counts as up) | `unexpected_light` | YELLOW |
| dark | up | `prom_auth` | `unexpected_light` | YELLOW |
| dark | not up | `http_timeout` | `expected_dark` | GREEN |
| light | up | `tcp_connect_refused` (a `ts_reach` refusal is up) | none | GREEN |
| light | up | `metrics_body_unexpected` | none | YELLOW |
| light | down | `http_unexpected_status` | `unexpected_dark` | YELLOW |
| light | unknown | `tcp_connect_timeout` | `probe_unknown` | YELLOW |
| any | unknown | none (`vantage_blind` alone) | `vantage_blind` | RED |
| any | any | any | `held` (vantage not blind) | GREEN |

## 6. Derivation

- `hosts[].severity` is the worst severity among that host's checks.
- `hosts[].reasons` is the union of `reasons[]` from the host's worst-severity checks, taken in (kind, target) order, first occurrence kept, so a GREEN host lists the reasons of all its checks.
- `overall` is the worst host severity.
- Config requires at least one host and at least one check on every host, and the probe schema requires `hosts` to have at least one entry, so `overall` is always derived from something observed and is never vacuous.
- Held and expected-dark checks are GREEN, so they never raise any rollup.
- **Blind:** every check and host is RED, so `overall` is RED by plain derivation. The only special case is the forced `unknown` results (section 5, rule 1), which a test pins.
- Hosts are sorted by id and checks by (host, kind, target), bytewise ascending. No map iteration reaches the JSON.

## 7. Vantage

`vantage.status` is one of:

- `ok`: the local tailnet address is present and at least one canary succeeded.
- `unchecked`: the local tailnet address is present and no canary is configured.
- `blind`: the local tailnet address is absent, or canaries are configured and every one failed.

The local test looks for an address in `100.64.0.0/10` on an interface; the remote test runs each canary. No `tailscale` CLI is needed. Blind exits `5` with the document written.

Config shape: `vantage: {id: <slug>, canaries: [{host, kind, target}]}`. A canary names a configured check by its (host, kind, target) triple, and the kind must be `ts_reach`. The `target` is the check's **effective** target after the kind's default port is applied (a `ts_reach` check with no `port` is `:9100`), before any overlay override. No canaries means `unchecked`.

Test note: a canary written `:9100` resolves to a `ts_reach` check configured with no `port`; a canary `:9101` does not resolve to that check and fails `validate`; a canary naming a check that does not exist, or a non-`ts_reach` kind, fails `validate`.

## 8. Files

Config (`simplex.config/v0`), public, slugs only. Strict decoding: unknown keys and unknown major versions are errors.

```yaml
apiVersion: simplex.config/v0
vantage:
  id: lathe
  canaries:
    - {host: tower, kind: ts_reach, target: ":9100"}
hosts:
  - id: tower
    checks:
      - {kind: ts_reach, expected: light}              # port optional
      - {kind: node_exporter, expected: light}
      - {kind: kube_apiserver, expected: dark}
  - id: box
    checks:
      - {kind: prom_up, expected: light, path: /metrics}  # allowlist only
holds:
  - host: tower              # slug or "*"
    kind: ts_reach           # kind or "*"
    reason: planned reboot   # required, scrubbed in CI
    until: 2026-11-20T00:00:00Z  # RFC 3339, required
```

| Field | Required | Rule |
| --- | --- | --- |
| `apiVersion` | yes | `const simplex.config/v0` |
| `vantage.id` | yes | slug |
| `vantage.canaries` | no | check references; each names a configured `ts_reach` check |
| `hosts[]` | yes, at least 1 | each `id` unique |
| `hosts[].checks[]` | yes, at least 1 | `kind` and `expected` required; `port` optional; `path` only on `prom_up` |
| `holds[]` | no | `host`, `kind`, `reason`, `until` all required |

`example.yaml` in the repo root is a config document with public slugs only. It validates against the config schema in CI.

Overlay (`simplex.overlay/v0`): public shape, private values, never tracked (`simplex.overlay.yaml`, `*.overlay.yaml`). Path from `--overlay` or `SIMPLEX_OVERLAY`. Keyed by host slug.

```yaml
apiVersion: simplex.overlay/v0
hosts:
  box:
    address: "<tailnet address or name>"
    lan_address: "<optional>"
    overrides:                 # optional, per (kind, target)
      - {kind: prom_up, target: ":9090/-/ready", path: /metrics}
```

An override matches the host's check by `kind`, and by `target` when given; `port` and `path` it sets replace the config's values, and the document `target` shows the effective form. A check that needs an address with no overlay entry fails closed: exit `3`, naming the missing slugs. Expired holds are ignored and `validate` warns.

## 9. Document

```json
{
  "apiVersion": "simplex.probe/v0",
  "generated_at": "2026-11-13T18:00:00Z",
  "vantage": {"id": "lathe", "status": "ok"},
  "overall": "YELLOW",
  "hosts": [
    {"id": "box", "severity": "YELLOW", "reasons": ["http_unexpected_status", "prom_pair_degraded", "unexpected_dark"]},
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

`generated_at` is UTC, second precision, ending in `Z`. `--now` pins it and the hold clock together. Additive changes within `v0`; consumers refuse unknown majors via the required `apiVersion`.

## 10. Exit codes

| Code | Meaning | Document written |
| --- | --- | --- |
| 0 | GREEN | yes |
| 1 | YELLOW | yes |
| 2 | RED | yes |
| 3 | config or usage error | no |
| 4 | runtime or I/O error | no |
| 5 | vantage blind (`overall: RED`) | yes |

No error path exits 0, 1 or 2. Under `--json`, every non-zero error exit (3, 4) writes one `{"error":{"code","message"}}` line to stderr. `serve` never exits on severity: `0` normally, `3` or `4` for config, bind or I/O. Only `main` maps errors to codes.

`validate --json` (S1) prints `{"findings":[{"level","path","message","line"}]}` to stdout, `level` being `warning` or `error`. It exits `0` with only warnings and `3` with any error; the error line still goes to stderr.

## 11. Document file and fixtures

Consumers such as Vigil read the document from a file and never run `simplex`.

- `--out PATH` on `check` and `serve` writes the `--json` document to `PATH`: temp file in the same directory, `fsync`, atomic `rename`. A reader never sees a partial document. Mode `0644`. Slugs only.
- Exits `0`, `1`, `2` and `5` write the document. Exits `3` and `4` never write, so the last good file stays in place and a reader detects staleness from `generated_at`.
- **Writer of record is `serve --out`.** It uses the same snapshot as `/metrics` and writes every interval. A timer running `check --json --out` is the supported fallback.
- A failed write is an I/O error: exit `4`, and the temp file is removed.

**Fixture layout (frozen):** one directory per case, `testdata/<case>/{config.yaml,document.json,exit}`. `exit` holds the integer exit code and a newline. `document.json` is present exactly when `exitcode.WritesDocument(exit)` is true, so the error cases (exit 3 and 4) have `config.yaml` and `exit` only. Documents are built as if `--now 2026-11-13T18:00:00Z`. Schema-level negative cases live under `internal/conformance/testdata/`, not `testdata/`.

S1 tests for `--out`: the temp file is in the target directory; the target is replaced by rename, never truncated in place; a prior good file is byte-identical after an exit 3 or 4 run; exit 5 writes a document with `vantage.status: blind`; mode is `0644`; no temp file remains after success or failure; `serve --out` writes the bytes the `/metrics` snapshot was built from.

## 12. CLI contract (frozen parts)

The rest of this section belongs to the CLI slice.

- Commands: `check`, `validate`, `serve`, `version`. Global flags: `--config`, `--overlay`, `--json`, `--now`, `--out`.
- `check` prints the table by default; `--json` prints the `simplex.probe/v0` document. Exit codes are identical in both modes.
- Table columns, in order: `HOST | CHECK | EXPECTED | RESULT | SEVERITY | REASONS`. Reasons are joined with `, ` and truncated to `+N` when the column would overflow. `NO_COLOR` disables color; severity is never carried by color alone.
- `validate` prints `path[idx].field: message (config line)`, exit `0` with warnings, `3` with errors.

## 13. Naming freeze

Frozen at S0: commands `check`, `validate`, `serve`, `version`; flags `--json`, `--now`, `--out`, `--config`, `--overlay`; schema file names; document and config field names; the check kinds and reason codes above; the `simplex_` metric prefix. Changes after S1 are breaking.

## 14. Public-tree scrub

Every tracked text file is scanned in CI. The allowlist is exact; anything else fails the job.

| Allowed | Why |
| --- | --- |
| `127.0.0.0/8` (any 127.x address) | loopback, in tests and the loopback listen default |
| `0.0.0.0` | wildcard bind, mentioned only to forbid it |
| `255.255.255.255` | broadcast literal, documentation |
| the CIDR literal `100.64.0.0/10` | CGNAT range, needed by blind detection |

- The three TEST-NET blocks of RFC 5737 are warnings, allowed only in docs and fixtures as placeholders.
- Everything else fails: other IPv4 and IPv6 literals, `*.ts.net` and `*.tailnet` names, `*.local`, `*.lan`, private keys, GitHub, cloud and Slack tokens, JWTs, bearer tokens, and `/Users/...` or `/home/...` paths.
- Emails must end in `example.com`, `example.org` or `users.noreply.github.com`.
- No tracked file whose base name contains `overlay`, except the overlay schema.
