# Changelog

All notable changes to **ddo-mikrotik** are recorded here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

ddo-mikrotik is a webhook sidecar for [docker-dns-operator](https://github.com/mrkhachaturov/docker-dns-operator), implementing the [external-dns webhook provider v1 contract](https://kubernetes-sigs.github.io/external-dns/latest/docs/tutorials/webhook-provider/) against the native RouterOS binary API. The same sidecar works with the upstream kubernetes-sigs/external-dns controller.

## [Unreleased]

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

[Unreleased]: https://github.com/mrkhachaturov/ddo-mikrotik/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/mrkhachaturov/ddo-mikrotik/releases/tag/v0.1.0
