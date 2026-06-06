# Changelog

All notable changes to **ddo-mikrotik** are recorded here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

ddo-mikrotik is a webhook sidecar for [docker-dns-operator](https://github.com/mrkhachaturov/docker-dns-operator), implementing the [external-dns webhook provider v1 contract](https://kubernetes-sigs.github.io/external-dns/latest/docs/tutorials/webhook-provider/) against the native RouterOS binary API. The same sidecar works with the upstream kubernetes-sigs/external-dns controller.

## [0.2.0] — 2026-06-07

### Added
- Wildcard DNS support (A/AAAA/CNAME and other supported types). An endpoint with a `*.`-prefixed `DNSName` (e.g. `*.dev.example.com`) is now written as a single RouterOS `/ip/dns/static` row using the `regexp` field instead of a literal `name`. RouterOS treats a literal asterisk in `name` as a dead, non-matching hostname; the `regexp` form (`^.*\.dev\.example\.com$`) correctly matches subdomain queries (`foo.dev.example.com` and deeper). The ownership `comment` is stamped on the regexp row exactly as on literal rows.

### Fixed
- Wildcard records no longer churn (infinite create/diff every reconcile tick). `ListRecords` now reverse-maps our own regexp rows back to the `*.…` wildcard `DNSName`, so the operator sees the record as present. Only regexp rows that match the exact shape this sidecar writes **and** carry our ownership `comment` are surfaced; arbitrary user-authored regexp rows are still skipped on read and never modified.
- `GET /` now emits the domain filter as `include` (per external-dns `endpoint.DomainFilter`) instead of the legacy `filters` key, which upstream parses as an unset filter — previously every record was routed through the sidecar regardless of zone.

## [0.1.1] — 2026-05-25

### Added
- `healthcheck` subcommand on the webhook binary. Invoked as `/usr/local/bin/webhook healthcheck`, performs a local `GET /healthz` (using `WEBHOOK_LISTEN` for the address) and exits `0` on a 2xx response, `1` otherwise.
- `HEALTHCHECK` directive in the Dockerfile wired to that subcommand. The image is distroless (no shell), so the binary is its own probe — the canonical distroless Go pattern, avoiding a second artifact to build/sign/SBOM.

## [0.1.0] — 2026-05-25

First tagged release.

### Added
- External-dns webhook provider v1 endpoints: `GET /`, `GET /records`, `POST /records`, `POST /adjustendpoints`, `GET /healthz`.
- MikroTik DNS via the native RouterOS binary API — port 8728 cleartext (`MIKROTIK_USE_TLS=false`) or 8729 api-ssl (`MIKROTIK_USE_TLS=true`). REST is intentionally not used; the binary API is older, more stable, and available on every RouterOS version.
- Records supported: A, AAAA, CNAME, MX, NS.
- Optional zone allow-list via `MIKROTIK_ZONES` (comma-separated; empty = accept any FQDN — MikroTik has no native zone concept).
- Self-signed cert support via `MIKROTIK_SKIP_TLS_VERIFY=true` for the api-ssl service (lab use).
- Credentials loadable via `MIKROTIK_USERNAME` / `MIKROTIK_PASSWORD` (env) or the matching `*_FILE` variants (Docker secrets).
- Ownership round-trip through the row's `comment` field — the operator stamps `labels.owner` on each Endpoint, the sidecar persists it verbatim and reads it back on `GET /records`. Two operators with different `INSTANCE_ID`s can safely share the same router.

### Notes
- Distroless image, pure Go, CGO disabled.
- Requires a dedicated RouterOS user with `read+write+api` policies (console/SSH/Winbox/REST should be denied). See [README.md](README.md) for the step-by-step user setup.

[Unreleased]: https://github.com/mrkhachaturov/ddo-mikrotik/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/mrkhachaturov/ddo-mikrotik/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/mrkhachaturov/ddo-mikrotik/releases/tag/v0.1.1
[0.1.0]: https://github.com/mrkhachaturov/ddo-mikrotik/releases/tag/v0.1.0
