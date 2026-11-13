# Simplex semantics (S0 contract)

This is the prose contract for `simplex.probe/v0`, `simplex.config/v0` and `simplex.overlay/v0`. The Go vocabulary lives in `internal/contract`; the schemas live in `schema/`. When the three disagree, a test fails. Anything marked **provisional** is not frozen at S0.

## 1. Principles

- Simplex reports what it can observe directly. It never reads alert state, and it never checks disk.
- `check` is stateless. No hold timers, no memory between runs.
- `result` is the observation. `severity` is the judgment. They are separate fields and never substitute for each other.
- `hosts[].severity` and `overall` are derived from checks. They are never assigned on their own.
- Public config, fixtures, docs and labels carry host slugs only. Addresses live in the private overlay.

## 2. Check kinds

The enum is open: adding a kind is a minor change. A check instance is identified by (host slug, kind, target).

| Kind | Probe | Default port | Target form | Default `expected` |
| --- | --- | --- | --- | --- |
| `ts_reach` | TCP connect over the tailnet | 9100 | `:9100` | light |
| `node_exporter` | `GET /metrics` over the tailnet | 9100 | `:9100/metrics` | light |
| `prom_up` | `GET <path>` on a Prometheus | 9090 | `:9090/-/ready` | light |
| `kube_apiserver` | TCP connect over the tailnet | 6443 | `:6443` | dark |
| `lan_reach` (provisional) | TCP connect over the LAN address | 22 | `:22` | light |
| `ssh` (provisional) | TCP connect plus `SSH-` banner on a named path | 22 | `:22` | light |

- `target` is `:<port>` plus the path for HTTP kinds. It never contains an address.
- Port 18080 is never dialed. Config load rejects any check that names it, and the dialer refuses it as well.
- `prom_up` path comes from an allowlist: `/metrics`, `/-/ready`, `/-/healthy`. Default `/-/ready`. HTTP only, host must be an overlay slug, no userinfo, no query, no redirects, 1 MiB body cap.
- `lan_reach` and `ssh` land in the third S1 slice. Their rows are intent, not freeze. The kind names are frozen.

## 3. Result classification

`down` is a definitive negative. `unknown` is indeterminate: a timeout, an unreachable network, or a DNS failure. A timeout never serializes as `down`.

| Kind | `up` | `down` | `unknown` |
| --- | --- | --- | --- |
| `ts_reach`, `lan_reach` | connect accepted; connect refused (the host answered, so the path is alive) | none | timeout, ENETUNREACH or EHOSTUNREACH, DNS error |
| `node_exporter` | HTTP 200 | connect refused; HTTP 404 or 5xx | timeout, unreachable, DNS error |
| `kube_apiserver` | connect accepted | connect refused | timeout, unreachable, DNS error |
| `prom_up` | 200; 503 on `/-/ready`; 401 or 403; 200 on `/metrics` without `prometheus_build_info` in the first 64 KiB | connect refused; HTTP 404 or 5xx | timeout, unreachable, DNS error |
| `ssh` (provisional) | banner starts with `SSH-` | connect refused; other banner | timeout, unreachable, DNS error |

Retry: one retry after 250 ms, on timeout only. Never retry a refusal.

## 4. Reason codes

Closed, snake_case, frozen at S0. Add a code is a minor change; remove or rename is breaking. `watchdog_only` and `peer_prometheus_*` are not Simplex codes.

Each probed check carries exactly one **outcome** code, plus zero or more **verdict** codes. Under a blind vantage a check carries only `vantage_blind`. `reasons[]` is sorted bytewise ascending.

| Code | Class | Emitted when |
| --- | --- | --- |
| `tcp_connect_ok` | outcome | TCP connect accepted |
| `tcp_connect_refused` | outcome | TCP connect refused |
| `tcp_connect_timeout` | outcome | TCP connect timed out |
| `unreachable` | outcome | network or host unreachable |
| `dns_error` | outcome | name resolution failed |
| `http_ok` | outcome | HTTP 200 (and body check passed where one applies) |
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

Evaluated per check, in this order. The first rule that matches decides.

1. **Blind vantage:** result `unknown`, severity YELLOW, reasons `[vantage_blind]`. This overrides holds and expected-dark.
2. **Held** (an unexpired hold matches host and kind): severity GREEN, reasons gain `held`, whatever the result. The check still runs and reports its real result.
3. **Expected dark:** not up gives GREEN `expected_dark`. Up gives YELLOW `unexpected_light`.
4. **Expected light:**
   - up: GREEN. The outcome codes `prom_not_ready`, `prom_auth` and `metrics_body_unexpected` raise it to YELLOW, with no verdict code.
   - down: YELLOW `unexpected_dark`.
   - unknown: YELLOW `probe_unknown`. Never RED on its own.
5. **Prometheus pair:** over the non-held, expected-light `prom_up` checks. If there are two or more and none is up, each gets RED `prom_pair_down`. If at least one is not up and at least one is up, each not-up check gets YELLOW `prom_pair_degraded`. The pair verdict replaces `unexpected_dark` and `probe_unknown` on those checks. With a single such check, rule 4 applies.

An unexpected `node_exporter` that is not up is YELLOW and never RED on its own.

A held `prom_up` check is excluded from the pair count. It cannot raise a rollup, and the remaining member, if not up, is YELLOW `prom_pair_degraded` rather than RED. *Resolved at S0 for schema review: the spec says held checks never raise any rollup and does not cover this overlap.*

## 6. Derivation

- `hosts[].severity` is the worst severity among that host's checks.
- `hosts[].reason` is the first reason of the host's worst-severity checks, taking the check order (kind, target) and, within a check, the verdict code if there is one, otherwise the outcome code. It is `""` when the host is GREEN.
- `overall` is the worst host severity.
- Held and expected-dark checks are GREEN, so they never raise any rollup.
- **Blind exception:** under a blind vantage every check and host is YELLOW and `overall` is RED. This is the one case where `overall` is not the worst host. A test pins it.
- Check order, host order: bytewise ascending by (host, kind, target). No map iteration reaches the JSON.

## 7. Vantage

`vantage.status` is one of:

- `ok`: the local tailnet address is present and at least one configured canary succeeded.
- `unchecked`: the local tailnet address is present and no canary is configured.
- `blind`: the local tailnet address is absent, or canaries are configured and every one failed.

The local test looks for an address in the CGNAT range (RFC 6598) on an interface; the remote test is a `ts_reach` to each canary host. No `tailscale` CLI is needed. Blind exits `5` with the document written, `overall: RED`.

## 8. Files

Config (`simplex.config/v0`), public, slugs only. Strict decoding: unknown keys and unknown major versions are errors.

```yaml
apiVersion: simplex.config/v0
vantage:
  id: lathe
  canaries: [tower]          # optional host slugs
hosts:
  - id: tower
    checks:
      - kind: ts_reach       # port optional, default per kind
      - kind: node_exporter
      - kind: kube_apiserver # expected defaults to dark for this kind
  - id: box
    checks:
      - kind: prom_up
        path: /metrics       # allowlist only
holds:
  - host: tower              # slug or "*"
    kind: ts_reach           # kind or "*"
    reason: planned reboot   # required, scrubbed in CI
    until: 2026-11-20T00:00:00Z  # RFC 3339, required
```

Overlay (`simplex.overlay/v0`), private, never tracked (`simplex.overlay.yaml`, `*.overlay.yaml`). Path from `--overlay` or `SIMPLEX_OVERLAY`, never an argv value that carries an address.

```yaml
apiVersion: simplex.overlay/v0
hosts:
  tower: {address: "<tailnet address or name>", lan_address: "<optional>"}
```

A check that needs an address with no overlay entry fails closed: exit `3`, naming the missing slugs. Expired holds are ignored and `validate` warns.

## 9. Document

```json
{
  "apiVersion": "simplex.probe/v0",
  "generated_at": "2026-11-13T18:00:00Z",
  "vantage": {"id": "lathe", "status": "ok"},
  "overall": "YELLOW",
  "hosts": [
    {"id": "box", "severity": "YELLOW", "reason": "prom_pair_degraded"},
    {"id": "lathe", "severity": "GREEN", "reason": ""}
  ],
  "checks": [
    {"host": "box", "kind": "prom_up", "target": ":9090/metrics", "result": "down",
     "severity": "YELLOW", "expected": "light",
     "reasons": ["http_unexpected_status", "prom_pair_degraded"]},
    {"host": "lathe", "kind": "prom_up", "target": ":9090/-/ready", "result": "up",
     "severity": "GREEN", "expected": "light", "reasons": ["http_ok"]}
  ]
}
```

`generated_at` is UTC, second precision, ending in `Z`. `--now` pins it and the hold clock together. Additive changes within `v0`; consumers refuse unknown majors via `apiVersion`.

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

## 11. Document file

Consumers such as Vigil read the document from a file and never run `simplex`.

- `--out PATH` on `check` and `serve` writes the `--json` document to `PATH`.
- Write order: temp file in the same directory as `PATH`, `fsync`, atomic `rename`. A reader never sees a partial document. Mode `0644`. Slugs only.
- Exits `0`, `1`, `2` and `5` write the document. Exits `3` and `4` never write, so the last good file stays in place and a reader detects staleness from `generated_at`.
- **Writer of record is `serve --out`.** It uses the same snapshot as `/metrics` and writes every interval. A timer running `check --json --out` is the supported fallback.
- A failed write (unwritable directory, failed rename) is an I/O error, exit `4`, and removes its temp file.

### Fixture and test notes

- Every fixture directory under `testdata/` holds `probe.json` (when the exit code writes a document) and `expect.json`: `{"exit_code": N, "writes_document": true|false}`. `writes_document` must equal `exitcode.WritesDocument(exit_code)`.
- Add two error fixtures with no `probe.json`: a config error (exit 3) and an I/O error (exit 4).
- S1 tests for `--out`: the temp file lives in the target directory; the target is replaced by rename, never truncated in place; a prior good file is byte-identical after an exit 3 or 4 run; exit 5 writes a document with `vantage.status: blind`; the file mode is `0644`; no temp file remains after success or failure; `serve --out` writes the same bytes the `/metrics` snapshot was built from.

## 12. CLI contract

*To be written by the CLI slice: table columns and truncation, `--help` bar, `validate` output, `version` output, error message style, global flags.*

## 13. Naming freeze

Frozen at S0: commands `check`, `validate`, `serve`, `version`; flags `--json`, `--now`, `--out`, `--config`, `--overlay`; schema file names; document and config field names; the check kinds and reason codes above; the `simplex_` metric prefix. Changes after S1 are breaking.
