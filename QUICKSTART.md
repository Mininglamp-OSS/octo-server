# QUICKSTART — OCTO Server

`octo-server` is the Go backend at the centre of OCTO. There are two
recommended ways to get a running instance:

1. **One-shot Docker Compose stack** (server + admin + web + matter +
   smart-summary + WuKongIM + MySQL + Redis + MinIO + nginx, all wired
   up): use the official OOTB deployment at
   [`Mininglamp-OSS/octo-deployment`](https://github.com/Mininglamp-OSS/octo-deployment).
   That repository is the single source of truth for OOTB deployment.
2. **Local Go build against your own infra**: clone this repo, build
   the binary, and point it at a WuKongIM + MySQL you already run.

The first option is the right one if you "just want to try OCTO". The
second is for backend developers iterating on `octo-server` itself.

---

## Option 1 — One-shot Docker Compose (recommended for trial)

Follow the walkthrough in
[`Mininglamp-OSS/octo-deployment`](https://github.com/Mininglamp-OSS/octo-deployment):

```bash
git clone https://github.com/Mininglamp-OSS/octo-deployment.git
cd octo-deployment
./setup.sh                               # interactive, generates docker/.env
cd docker
docker compose up -d
docker compose ps                        # all services should reach (healthy)
```

The stack listens on `http://${OCTO_DOMAIN}:${OCTO_HTTP_PORT}`
(default `http://octo.local:28080`). See
[`docker/README.md` in octo-deployment](https://github.com/Mininglamp-OSS/octo-deployment/blob/main/docker/README.md)
for the prerequisites checklist, the pre-flight warning when another
OCTO stack already runs on the same host, and the full environment-
variable contract.

> The `docker/octo/` and `docker/tsdd/` compose stacks that used to
> live inside this repository have been **removed**. They duplicated a
> subset of `octo-deployment` while drifting behind it (no preflight,
> no minio-init secret rotation, no rate-limited nginx vhost, no
> matter/summary services), and keeping two on-disk copies meant fixes
> only landed in one of them. `octo-deployment` is now the single
> source of truth for OOTB deployment.

---

## Option 2 — Local Go build against your own infra

### Prerequisites

- Go ≥ 1.25 (see `go.mod`)
- A reachable WuKongIM ≥ v2 instance
- A reachable MySQL 8 with the schema applied
- A reachable Redis 7
- (Optional) An S3-compatible object store for the file modules

### Build

```bash
git clone https://github.com/Mininglamp-OSS/octo-server.git
cd octo-server
go build ./...
```

If `go build` fails with "missing go.sum entry" against a sibling OCTO
module, see [`BUILDING.md`](./BUILDING.md) for the cross-repo `replace`
workaround.

### Configure

Copy a config template from `config/` (or write your own `tsdd.yaml`)
and point each section at your live infra:

- `db.mysqlAddr` — your MySQL DSN
- `db.redisAddr` — your Redis address
- `wukongIM.url` and `wukongIM.managerToken` — your WuKongIM control
  plane
- `minio.*` (or whichever object-storage adapter you use) — your S3
  endpoint, app credentials, and bucket layout

### Run

```bash
./octo-server api --config /path/to/tsdd.yaml
```

Smoke check:

```bash
curl http://localhost:8090/v1/ping        # {"status":"ok"} on success
```

### Register your first user

Open the OCTO web SPA in your browser (the OOTB stack mounts it at
`/`; with a custom deploy, point it at whatever URL fronts the web
container). Or call the REST API directly:

```bash
curl -X POST http://localhost:8090/v1/user/register \
  -H "Content-Type: application/json" \
  -d '{"phone":"+8613800000000","password":"test1234","name":"Admin"}'
```

### Connect an AI Agent

Install the daemon CLI:

```bash
go install github.com/Mininglamp-OSS/octo-daemon-cli@latest
```

In OCTO, send `/daemon` to BotFather to receive your start command.

## Troubleshooting

- **Port conflicts** in the OOTB stack: override `OCTO_HTTP_PORT`,
  `OCTO_WEB_PORT`, etc. in `docker/.env` (see the
  `octo-deployment` README for the full list).
- **WuKongIM unhealthy**: confirm `wk.yaml`'s `tokenAuthOn` /
  `managerToken` match `octo-server`'s `wukongIM.managerToken` —
  drift between the two is the most common cause.
- **Go build fails with "missing go.sum entry for octo-lib"**:
  See [BUILDING.md](./BUILDING.md) for the cross-repo `replace`
  workaround.

## Stop & reset (OOTB stack)

```bash
# ⚠ Pre-flight: read the matching section in
#   https://github.com/Mininglamp-OSS/octo-deployment/blob/main/docker/README.md
#   before any down -v on a host that may also run another OCTO stack.
cd /path/to/octo-deployment/docker
docker compose down                       # stop containers, keep data
docker compose down -v                    # stop + delete volumes (DESTRUCTIVE)
```
