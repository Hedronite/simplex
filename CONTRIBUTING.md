# Contributing to Simplex

Thank you for looking at Simplex. This document covers how to propose changes.

## Ground rules

- Inspect-only: the probe never mutates a host and never reads alert state.
- The public tree carries host slugs only. Addresses, hostnames and secrets
  live in private overlay files that are never committed. CI scrubs every
  tracked file; the allowlist is fixed in `docs/semantics.md` §14.
- The prose contract in `docs/semantics.md` and the schemas under `schema/`
  are the source of truth. When code, schema and prose disagree, a test
  fails. Vocabulary marked frozen there changes only via a breaking change.

## Reporting issues

Open a GitHub issue for bugs and questions. For security-sensitive reports,
do not open a public issue — follow `SECURITY.md`.

## Development

- Go 1.27 or newer.
- Before opening a pull request, run `go vet ./...` and `go test ./...`.
- Keep changes hermetic: tests never dial a live mesh; use loopback and
  fixtures (`testdata/<case>/{config.yaml,document.json,exit}`).
- Public documentation uses slugs and the RFC 5737 TEST-NET placeholders
  only.

## Pull requests

- Fork or branch, make your change, and open a pull request against `main`.
- One pull request per concern; keep diffs readable.
- CI runs vet, lint, vulnerability scanning, the race detector and the
  public-tree scrub. All of these must pass before merge.

## Developer Certificate of Origin

Every commit must be signed off under the [DCO](https://developercertificate.org/).
Sign your commits with `git commit -s`; the sign-off trailer
(`Signed-off-by: Name <email>`) asserts you have the right to submit the work
under the project's Apache-2.0 license.

Commit messages describe what changed and why. Keep the subject and body free
of infrastructure references; this is a public repository.

## License

By contributing, you agree that your contributions are licensed under the
Apache-2.0 license (`LICENSE`).
