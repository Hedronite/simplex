# Security Policy

## Reporting a vulnerability

Please report security issues privately using GitHub's private vulnerability
reporting at
<https://github.com/Hedronite/simplex/security/advisories/new>.

Do not open a public issue for a security report. Include a description of the
issue, how to reproduce it, and the impact you believe it has. You will get an
acknowledgment and, as the fix progresses, updates in the advisory thread.

## Supported versions

The current `main` branch is supported, and `0.1.0` is the first tagged
release. This section lists the supported minor versions and states which get
fixes as releases accumulate.

## Scope

In scope:

- The `simplex` binary: config and overlay parsing, probe behavior, HTTP and
  TCP probe paths, the `/metrics` listener, and document writing.
- Anything that would let a probe run escape its inspect-only boundary
  (mutating a host, reading alert state, dialing denied ports).
- Anything that leaks addresses, hostnames or secrets into the public tree,
  the document, or metric labels.

Out of scope:

- The hosts and services being probed; a compromised target cannot escalate
  through Simplex (probes send no credentials, and `prom_up` follows no
  redirects and accepts no response body beyond its 1 MiB cap).
- The mesh network itself, DNS, or the metrics stack that scrapes Simplex.
- Social engineering of operators.

## Design guarantees you can rely on

- Listeners bind loopback by default; a wider bind requires an explicit
  unsafe flag.
- Secrets and addresses are read from a private overlay file or the
  environment, never from command-line values.
- Port 18080 is never dialed, at config load or in the dialer.
- The public repository carries slugs only; CI fails on addresses, hostnames
  and secrets in tracked files.

## Disclosure policy

Confirmed issues get a fix on `main` and, once releases exist, on all
supported versions. Advisories are published on this repository; reporters
are credited in the advisory unless they prefer otherwise.
