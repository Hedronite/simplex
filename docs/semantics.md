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

- `target` is `:<port>` plus the path for HTTP kinds. It never contains an address. The document always carries the resolved `target`; config may omit it and accept the kind default.
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

Classification and severity are separate layers, in this order and no other:

1. **Outcome layer.** Every probed check carries exactly one outcome code, chosen by the table above. Outcome codes describe; they never carry severity. They never replace or suppress a verdict code.
2. **Verdict layer.** §5 decides severity and, per check, at most one verdict code. A check may be YELLOW with zero verdict codes (`prom_not_ready`, `prom_auth`, `metrics_body_unexpected` are enough on their own).
3. **Pair layer.** §5.5 may replace the verdict on `prom_up` checks and raise severity to RED.
4. **Derivation.** §6 rolls checks up to hosts and hosts up to `overall`.

## 4. Reason codes

Closed, snake_case, frozen at S0. Add a code is a minor change; remove or rename is breaking. Alert-derived codes are never emitted by Simplex: alert state belongs to Augur, and a code describing a state Simplex cannot observe would be a lie in a closed enum.

Each probed check carries exactly one **outcome** code, plus zero or one **verdict** code. The only check with no outcome code is one invalidated by a blind vantage, which carries `vantage_blind` alone. `reasons[]` is sorted bytewise ascending.

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
| `prom_pair_down` | verdict | every non-held, expected-light `prom_up` check is not up (two or more) |
| `prom_pair_degraded` | verdict | some, not all, non-held, expected-light `prom_up` checks are not up |
| `vantage_blind` | blind | the vantage cannot see the mesh |

## 5. Severity rules

Two passes over the same document: per-check rules in a fixed order (first match decides), then the pair pass. Holds and blindness are decided in the per-check pass.

1. **Blind vantage:** result `unknown`, reasons `[vantage_blind]`, severity RED. This overrides holds, expected-dark and the pair pass.
2. **Held** (an unexpired hold matches host and kind): severity GREEN, reasons gain `held`, whatever the result. The check still runs and reports its real result and its outcome code.
3. **Expected dark:** up gives YELLOW `unexpected_light`. Not up gives GREEN `expected_dark`.
4. **Expected light:**
   - up: GREEN. The outcome codes `prom_not_ready`, `prom_auth` and `metrics_body_unexpected` raise it to YELLOW, with no verdict code.
   - down: YELLOW `unexpected_dark`.
   - unknown: YELLOW `probe_unknown`. Never RED on its own.
5. **Prometheus pair (second pass):** see §5.5.

An unexpected `node_exporter` that is not up is YELLOW and never RED on its own.

### 5.5 Prometheus pair

Let P be every `prom_up` check in the document that is `expected: light` and has no unexpired hold. Held `prom_up` checks and any `expected: dark` `prom_up` check are outside P.

| P | Condition | Result |
| --- | --- | --- |
| 0 or 1 | any | The pair pass is silent. Rule 4 stands for the single member. |
| 2 or more | every member is not `up` | Each member gets severity RED and verdict `prom_pair_down`, replacing `unexpected_dark` or `probe_unknown`. |
| 2 or more | some members are not `up`, at least one is `up` | Each not-up member gets severity YELLOW and verdict `prom_pair_degraded`, replacing its own verdict. Up members are unchanged. |
| 2 or more | no member is not `up` | The pair pass is silent. |

- The pair verdict is written to the **checks**. `hosts[]` and `overall` follow by derivation (§6); RED is never assigned to a host directly.
- A held `prom_up` check is outside P, so held + down yields `prom_pair_degraded` YELLOW on the survivor, never RED. This resolves the overlap between "a hold never raises a rollup" and the pair rule.
- The pair pass never touches a check of another kind.

### 5.6 Combination rows

Pinned so the order cannot be guessed. `expected` from config, `result` and outcome from §3, then the first matching rule of §5.

| Expected | Result | Outcome code | Verdict | Severity |
| --- | --- | --- | --- | --- |
| dark | up | `http_ok` | `unexpected_light` | YELLOW |
| dark | up | `prom_not_ready` (503 on `/-/ready` counts as up) | `unexpected_light` | YELLOW |
| dark | up | `prom_auth` | `unexpected_light` | YELLOW |
| dark | not up | `http_timeout` | `expected_dark` | GREEN |
| light | up | `tcp_connect_refused` (`ts_reach` refusal is up) | none | GREEN |
| light | up | `metrics_body_unexpected` | none | YELLOW |
| light | down | `http_unexpected_status` | `unexpected_dark` | YELLOW |
| light | unknown | `tcp_connect_timeout` | `probe_unknown` | YELLOW |
| any | any | any | `held` | GREEN |
| any | unknown | none (`vantage_blind` alone) | `vantage_blind` | RED |

## 6. Derivation

Derivation runs after §5, from the finished check list. It is mechanical and has no exceptions.

- `hosts[].severity` is the worst severity among that host's checks.
- `hosts[].reason` is the code of the **first** check in that host's (kind, target) bytewise order among the checks whose severity equals `hosts[].severity`: the check's verdict code if it has one, otherwise its outcome code. It is `""` when the host is GREEN.
- A host with no checks has no `hosts[]` entry. No observation, no verdict.
- `overall` is the worst `hosts[].severity`, and `overall` is GREEN when `hosts[]` is empty.
- Held and expected-dark checks are GREEN, so they never raise any rollup.
- **Blind:** §5.1 makes every check RED, so every host is RED by the ordinary rule and `overall` is RED by the ordinary rule. There is no rollup exception to explain: blindness is a judgment on every check, not a shortcut on `overall`. Exit `5` is the signal that distinguishes blind from a mesh that is genuinely broken. A test pins a blind fixture to `overall: RED`, exit `5`, and every check and host RED with reason `vantage_blind`.
- Check order, host order: bytewise ascending by (host, kind, target). No map iteration reaches the JSON.

## 7. Vantage

Config:

```yaml
apiVersion: simplex.config/v0
vantage:
  id: lathe                # required, host slug shape
  canaries: [tower]        # optional, host slugs
```

- `vantage.id` is required. It is copied to `vantage.id` in the document.
- `vantage.canaries` is an optional list of host slugs, unique, ascending. Each slug must name a host in `hosts[]` that has exactly one `ts_reach` check instance. `validate` errors otherwise, because a canary that cannot be dialed would report `unchecked` forever.
- A canary is dialed as that host's `ts_reach` instance. There is no separate canary check kind.

`vantage.status` in the document is one of:

- `ok`: the local tailnet address is present and at least one configured canary succeeded.
- `unchecked`: the local tailnet address is present and no canary is configured.
- `blind`: the local tailnet address is absent, or canaries are configured and every one failed.

The local test looks for an address in the CGNAT range (RFC 6598) on an interface; the remote test is a `ts_reach` to each canary host. No `tailscale` CLI is needed. Blind exits `5` with the document written, `overall: RED`.

## 8. Files

### 8.1 Config (`simplex.config/v0`)

Public, slugs only, strict decoding: unknown keys and unknown major versions are errors. Required: `apiVersion`, `vantage`, `hosts`.

```yaml
apiVersion: simplex.config/v0
vantage:
  id: lathe
  canaries: [tower]          # optional host slugs
hosts:
  - id: tower
    checks:
      - kind: ts_reach       # target optional, default per kind
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

| Field | Required | Rule |
| --- | --- | --- |
| `apiVersion` | yes | `const simplex.config/v0` |
| `vantage.id` | yes | slug |
| `vantage.canaries` | no | unique host slugs, each with one `ts_reach` check |
| `hosts[]` | yes, 1 or more | each `id` unique |
| `hosts[].checks[]` | yes, 1 or more | `kind` required; `target`, `path`, `expected`, `timeout` optional |
| `hosts[].checks[].expected` | no | defaults per §2; the document always carries it |
| `holds[]` | no | `host`, `kind`, `reason`, `until` all required |

`example.yaml` in the repo root is a config document with public slugs only. It validates against the config schema in CI.

### 8.2 Overlay (`simplex.overlay/v0`)

Private, never tracked (`simplex.overlay.yaml`, `*.overlay.yaml`). Path from `--overlay` or `SIMPLEX_OVERLAY`, never an argv value that carries an address.

```yaml
apiVersion: simplex.overlay/v0
hosts:
  tower: {address: "<tailnet address or name>", lan_address: "<optional>"}
```

- The map is keyed by host slug, and the slug must exist in the config.
- `address` is required; `lan_address` is optional and only used by `lan_reach`.
- No other keys. Ports, paths, kinds and targets are public config, never overlay.
- A check that needs an address with no overlay entry fails closed: exit `3`, naming the missing slugs. Expired holds are ignored and `validate` warns.

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

- `apiVersion` is required and is `const simplex.probe/v0`. Consumers refuse unknown majors.
- `vantage.id` is required; `vantage.status` is the enum from §7.
- `hosts[].reason` is a reason code or the empty string (§6). `checks[].reasons[]` is never empty.
- `generated_at` is UTC, second precision, ending in `Z`. `--now` pins it and the hold clock together.
- Additive changes within `v0`. A new field, check kind or reason code is minor; removing, renaming or retyping one is breaking.

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

`validate` under `--json` writes its findings to stdout as `{"warnings":["<path>: <message>"],"errors":["<path>: <message>"]}`, exits `0` with warnings only and `3` with errors; diagnostics prose stays on stderr.

## 11. Document file

Consumers such as Vigil read the document from a file and never run `simplex`.

- `--out PATH` on `check` and `serve` writes the `--json` document to `PATH`.
- Write order: temp file in the same directory as `PATH`, `fsync`, atomic `rename`. A reader never sees a partial document. Mode `0644`. Slugs only.
- Exits `0`, `1`, `2` and `5` write the document. Exits `3` and `4` never write, so the last good file stays in place and a reader detects staleness from `generated_at`.
- **Writer of record is `serve --out`.** It uses the same snapshot as `/metrics` and writes every interval. A timer running `check --json --out` is the supported fallback.
- A failed write (unwritable directory, failed rename) is an I/O error, exit `4`, and removes its temp file.

### Fixture layout

Frozen at S0 so CI conformance and QA automation key off one shape.

- `testdata/<case>/exit`: the single integer exit code the case must produce, one line.
- `testdata/<case>/probe.json`: the golden document, present exactly when `exitcode.WritesDocument(code)` is true. No other file names are read.
- `testdata/<case>/config.yaml` and `testdata/<case>/overlay.yaml` when the case needs inputs. Fixtures use slugs and placeholder addresses only.
- There is no `expect.json`: `writes_document` is derived from the exit code, so the two can never disagree.
- The harness rule holds: a case whose `probe.json` implies a different code than its `exit` file fails. Blind implies `5`, otherwise `overall` maps GREEN/YELLOW/RED to `0`/`1`/`2`.
- Required cases at S0: green, one-prom-down, both-prom-down, blind, held, expired-hold, `prom_not_ready`, config error (exit `3`, no `probe.json`), I/O error (exit `4`, no `probe.json`).

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
- Everything else fails: other IPv4 and IPv6 literals, tailnet IPv6 prefixes, `*.ts.net` and `*.tailnet` names, `*.local`, `*.lan`, private keys, GitHub, cloud and Slack tokens, JWTs, bearer tokens, and `/Users/...` or `/home/...` paths.
- Emails must end in `example.com`, `example.org` or `users.noreply.github.com`.
- No tracked file whose base name contains `overlay`, except the overlay schema.
- The overlay is private user data: if a fix needs a real address, the fix is wrong.
