# ddo-mikrotik

A Go webhook provider for [`kubernetes-sigs/external-dns`](https://github.com/kubernetes-sigs/external-dns) that drives **MikroTik RouterOS** DNS via the native binary API (`/ip/dns/static`). Designed to run alongside [`docker-dns-operator`](https://github.com/mrkhachaturov/docker-dns-operator) as a sidecar — but the wire contract is the standard external-dns webhook v1, so any controller that supports it works.

## Why this exists

`docker-dns-operator` used to embed RouterOS support in-process. Moving each provider behind the external-dns webhook v1 wire lets the operator stay protocol-agnostic and lets the RouterOS code live with its own release cadence, its own tests, and its own deploy artefact. Internally this sidecar talks the native **RouterOS binary API** (8728 / 8729) rather than REST — the binary API is the supported, sequential, structured surface that ships with every RouterOS build.

## Wire contract

The sidecar implements the external-dns webhook provider v1 contract:

| Method | Path | Body | Response |
|---|---|---|---|
| GET | `/` | — | `{ "filters": ["<zone1>", ...] }` |
| GET | `/records` | — | `[]Endpoint` (every managed record, with `labels.owner` populated from the row's `comment`) |
| POST | `/records` | `Changes` `{ create, updateOld, updateNew, delete: []Endpoint }` | 204 on success, 4xx/5xx with `{"error": "..."}` body |
| POST | `/adjustendpoints` | `[]Endpoint` | echo (no-op) |
| GET | `/healthz` | — | 200 `{ok:true, status:"ready"}` or 503 with `status:"unreachable"` |

Accept/Content-Type for `/`, `/records`, `/adjustendpoints` is `application/external.dns.webhook+json;version=1`.

`Endpoint` mirrors `sigs.k8s.io/external-dns/endpoint.Endpoint`:

- `dnsName`, `recordType`, `recordTTL`, `targets`, `labels`, `setIdentifier`, `providerSpecific`
- Per record type, `targets` carries one element per record:
  - `A` / `AAAA`: an IP literal
  - `CNAME` / `NS`: a hostname (trailing dot optional)
  - `MX`: `"<priority> <server>"` in canonical zone-file form

## Ownership bridge

For every record this sidecar writes, the operator's `labels.owner` value (e.g. `docker-dns-operator:1`) is persisted **verbatim** into the row's `comment` field on `/ip/dns/static`. On `GET /records`, the sidecar reads the `comment` and surfaces it back as `labels.owner` on the returned Endpoint.

Critically: **the sidecar is ownership-agnostic**. It does NOT read `PROJECT_LABEL` or `INSTANCE_ID`. It does NOT invent its own owner string. It round-trips whatever the caller stamps on the payload. This lets two operator instances with different `INSTANCE_ID`s share the same RouterOS without stepping on each other — each only sees and manages rows whose comment matches its own label.

Rows with an empty `comment` are treated as **not managed**: they are excluded from `GET /records`, and creates / updates that would clobber them are refused as collisions.

## Required env vars

| Variable | Description |
|----------|-------------|
| `MIKROTIK_ADDRESS` | RouterOS API endpoint in `host:port` form (e.g. `192.168.1.1:8728`, or `:8729` when `MIKROTIK_USE_TLS=true`). |
| `MIKROTIK_USERNAME` *or* `MIKROTIK_USERNAME_FILE` | RouterOS API username, either inline or as a Docker secret file. |
| `MIKROTIK_PASSWORD` *or* `MIKROTIK_PASSWORD_FILE` | Matching password. |

## Optional env vars

| Variable | Default | Notes |
|---|---|---|
| `MIKROTIK_USE_TLS` | `false` | When `true`, dial the api-ssl service (typically `:8729`). |
| `MIKROTIK_SKIP_TLS_VERIFY` | `false` | Skip certificate validation. Only honoured when `MIKROTIK_USE_TLS=true`. Use only for self-signed router certs in lab environments. |
| `MIKROTIK_DEFAULT_TTL` | `3600` | TTL (seconds) applied when an inbound Endpoint omits `recordTTL`. Stored on the row as a RouterOS duration string. |
| `MIKROTIK_ZONES` | `""` | Comma-separated zone names returned verbatim on `GET /` as the DomainFilter, and used to constrain inbound writes (FQDN must end in one of them). Empty = accept any name. |
| `WEBHOOK_LISTEN` | `:9090` | HTTP bind address. |

## Record-type mapping

| Wire `recordType` | RouterOS fields used |
|---|---|
| `A` | `=type=A`, `=address=<ip>` |
| `AAAA` | `=type=AAAA`, `=address=<ipv6>` |
| `CNAME` | `=type=CNAME`, `=cname=<host>` |
| `NS` | `=type=NS`, `=ns=<host>` |
| `MX` | `=type=MX`, `=mx-exchange=<host>`, `=mx-preference=<int>` (the wire target `"10 mail.example.com"` is split here) |

TTL is converted between the wire (seconds, int64) and RouterOS' duration string format (`1h`, `30m1s`, `1d2h`).

## Health probe

`GET /healthz` issues a `/system/identity/print` against RouterOS and reports `200`/`503` accordingly. Use this as a Docker Compose / Kubernetes liveness target.

## Building locally

```bash
go build ./cmd/webhook
go test ./...
```

## Running with docker compose

```yaml
services:
  ddo-mikrotik:
    image: ddo-mikrotik:dev
    build: ./sidecars/ddo-mikrotik
    environment:
      WEBHOOK_LISTEN: ":9090"
      MIKROTIK_ADDRESS: 192.168.1.1:8728
      MIKROTIK_USERNAME: external-dns
      MIKROTIK_PASSWORD: ${MIKROTIK_PASSWORD}
      MIKROTIK_ZONES: home.lan

  docker-dns-operator:
    image: mrkhachaturov/docker-dns-operator:dev
    environment:
      WEBHOOK_MIKROTIK_URL: http://ddo-mikrotik:9090
      # ...
```

## License

MIT.
