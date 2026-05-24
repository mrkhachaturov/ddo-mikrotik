# ddo-mikrotik

The MikroTik sidecar for [docker-dns-operator](https://github.com/mrkhachaturov/docker-dns-operator). It owns the conversation with the router. The operator tells it which DNS records should exist; this process writes them into `/ip/dns/static` over RouterOS' native binary API.

One container, one router. Run a second container if you have a second router (one for home, one for office); the operator references each by its own env var.

## What it does

The sidecar is a small Go process with three jobs:

- Apply changes the operator sends (create, update, delete `/ip/dns/static` entries).
- Read the current state back so the operator can spot drift and reconcile.
- Tag every row it writes with the operator's instance label in the row's `comment` field, so two operators can share a router without overwriting each other's entries.

It does not own DNS records of its own. If the operator stops sending a record, the sidecar will delete it on the next cycle.

## How to configure

Required:

| Env | What it is |
|---|---|
| `MIKROTIK_ADDRESS` | `host:port` of the router's API endpoint, e.g. `192.168.1.1:8728` |
| `MIKROTIK_USERNAME` (or `MIKROTIK_USERNAME_FILE`) | RouterOS API user. Make a dedicated one; don't reuse `admin`. |
| `MIKROTIK_PASSWORD` (or `MIKROTIK_PASSWORD_FILE`) | Its password. |

Optional:

| Env | Default | Notes |
|---|---|---|
| `MIKROTIK_USE_TLS` | `false` | Use the api-ssl service (usually port 8729). |
| `MIKROTIK_SKIP_TLS_VERIFY` | `false` | Only for lab routers with self-signed certs. Honored only when `MIKROTIK_USE_TLS=true`. |
| `MIKROTIK_DEFAULT_TTL` | `3600` | Used when the operator sends a record without an explicit TTL. |
| `MIKROTIK_ZONES` | `""` | Comma-separated zone names. Constrains which FQDNs are accepted. Empty means "accept any name". |
| `WEBHOOK_LISTEN` | `:9090` | HTTP bind address. |

The RouterOS user needs `read` + `write` + `api` policies, and nothing more. Don't grant `policy=full` "just to make it work".

## How to run

Drop it next to the operator in compose:

```yaml
services:
  ddo-mikrotik:
    image: ddo-mikrotik:dev
    build: ./sidecars/ddo-mikrotik
    environment:
      MIKROTIK_ADDRESS: 192.168.1.1:8728
      MIKROTIK_USERNAME: external-dns
      MIKROTIK_PASSWORD: ${MIKROTIK_PASSWORD}
      MIKROTIK_ZONES: home.lan

  docker-dns-operator:
    image: mrkhachaturov/docker-dns-operator
    environment:
      WEBHOOK_MIKROTIK_URL: http://ddo-mikrotik:9090
```

The operator picks the sidecar up because of the `WEBHOOK_<NAME>_URL` pattern. The `<NAME>` part (here `MIKROTIK`) is what container labels then reference: `"providers": ["mikrotik"]`.

### Multiple routers

Run two containers, give the operator two env vars, route each record:

```yaml
services:
  ddo-mikrotik-home:
    image: ddo-mikrotik:dev
    environment:
      MIKROTIK_ADDRESS: 192.168.1.1:8728
      # ...

  ddo-mikrotik-office:
    image: ddo-mikrotik:dev
    environment:
      MIKROTIK_ADDRESS: 10.0.1.1:8728
      # ...

  docker-dns-operator:
    environment:
      WEBHOOK_MIKROTIK_HOME_URL: http://ddo-mikrotik-home:9090
      WEBHOOK_MIKROTIK_OFFICE_URL: http://ddo-mikrotik-office:9090
```

Then on a target container:

```yaml
labels:
  docker-dns-operator:1: |
    [
      { "type": "A", "name": "app.home.lan",   "address": "10.0.0.5", "providers": ["mikrotik-home"] },
      { "type": "A", "name": "app.office.lan", "address": "10.0.0.5", "providers": ["mikrotik-office"] }
    ]
```

Same image, two backends, no extra cleverness.

## How ownership tagging works

Every row this sidecar writes gets a value stamped into the row's `comment` field on `/ip/dns/static`. That value is whatever `labels.owner` arrives in the operator's request, written through verbatim. Two consequences:

The sidecar never touches a row whose comment doesn't match the calling operator's label. Other rows are visible on read but never modified or deleted. That covers manual entries you made by hand, and rows belonging to a different operator instance pointed at the same router.

The sidecar does not read `PROJECT_LABEL` or `INSTANCE_ID` itself. The operator decides the label; the sidecar persists it. The same sidecar image can be shared by any number of operators, each writing under its own identity. If a create lands on a name that already has a row with a different comment, the create is logged and skipped. No silent overwrite, ever.

## Record types

| Type | RouterOS fields used |
|---|---|
| A | `address` |
| AAAA | `address` |
| CNAME | `cname` |
| NS | `ns` |
| MX | `mx-preference`, `mx-exchange` (split from `"<priority> <host>"`) |

TTLs round-trip between integer seconds (the request) and RouterOS' duration strings (`1h`, `30m1s`, `1d2h`).

## Health probe

`GET /healthz` runs `/system/identity/print` against the router. 200 if it responds, 503 if it doesn't. Use it as a Docker Compose healthcheck.

## Local development

```bash
go build ./cmd/webhook
go test ./...
```

The HTTP layer is plain `net/http`. The RouterOS layer uses `github.com/go-routeros/routeros/v3` (binary API, ports 8728/8729). `internal/orchestrator/` is where the apply/list logic lives, including the comment-tag semantics. Everything else is glue.

## License

MIT.
