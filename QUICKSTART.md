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
go build -o octo-server .
```

`go build ./...` only compiles & checks every package; it does not
write any binaries (Go discards the object when more than one
package is built). To get a runnable `./octo-server`, build the root
main package explicitly with `-o octo-server .` as shown above.

If `go build` fails with "missing go.sum entry" against a sibling OCTO
module, see [`BUILDING.md`](./BUILDING.md) for the cross-repo `replace`
workaround.

### Configure

Copy the bundled template under `configs/` (e.g. `configs/tsdd.yaml`)
to your own path and point each section at your live infra:

- `db.mysqlAddr` — your MySQL DSN
- `db.redisAddr` — your Redis address
- `wukongIM.apiURL` and `wukongIM.managerToken` — your WuKongIM control
  plane (the YAML key is `apiURL`, not `url`; see `configs/tsdd.yaml`)
- `minio.*` (or whichever object-storage adapter you use) — your S3
  endpoint, app credentials, and bucket layout

Runtime language fallback is controlled by environment variables rather
than YAML. Set `OCTO_DEFAULT_LANGUAGE=zh-CN` in deployments so clients
that do not send `Accept-Language`, `lang`, or `i18n_lang` keep receiving
Chinese error messages during the i18n rollout. Supported values are
`zh-CN` and `en-US`; invalid values fail startup.

Trusted service-to-service language propagation is configured with
`OCTO_TRUSTED_LANG_HEADER_CIDRS`, a comma-separated CIDR list of direct
peer addresses allowed to supply `X-Octo-Lang`. When unset or empty, no
peer is trusted to set `X-Octo-Lang` and the header is ignored. If OCTO
runs behind trusted reverse proxies, also set `OCTO_TRUSTED_PROXY_CIDRS`;
the server will peel `X-Forwarded-For` from right to left through those
proxies before applying the trusted language CIDR check.

### Run

`octo-server` parses the `--config` flag with the stdlib `flag`
package, then dispatches on the first non-flag argument (`api` /
`config` / unset → API server). `flag.Parse()` stops at the first
positional, so `--config` must come **before** the subcommand:

```bash
# default config (configs/tsdd.yaml relative to working dir)
./octo-server api

# explicit config — note the flag goes before the subcommand
./octo-server --config /path/to/tsdd.yaml api
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

Install the daemon (requires Node.js ≥ 18):

```bash
npm install -g @mininglamp-oss/octo-daemon
```

In OCTO Web, open the **Runtimes** page from the left sidebar, click the
**+** button → **创建 Runtime** to get the install + start commands for your
machine. Copy and run them; the daemon will register with the server and
the runtimes will appear under your device in the tree.

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
