# OCTO Server — Docker deployment

One-shot, all-in-one OCTO stack for local development and small self-hosted
deployments. Brings up 11 containers behind an nginx reverse proxy:

| Service           | Role                          | Published port                    |
| ----------------- | ----------------------------- | --------------------------------- |
| `mysql`           | Shared MySQL 8.0              | `${OCTO_MYSQL_PORT}` (default 23306) |
| `redis`           | Session cache                 | `${OCTO_REDIS_PORT}` (26379)      |
| `minio`           | S3-compatible object storage  | `${OCTO_MINIO_API_PORT}` (29000)  |
| `wukongim`        | Realtime message bus          | `${OCTO_WK_WS_PORT}` (25200) etc. |
| `octo-server`     | Backend REST + event API      | `${OCTO_SERVER_PORT}` (28081)     |
| `nginx`           | Routing + landing page        | `${OCTO_HTTP_PORT}` (28080)       |
| `web`             | octo-web SPA                  | `${OCTO_WEB_PORT}` (28083)        |
| `admin`           | octo-admin console            | `${OCTO_ADMIN_PORT}` (28082)      |
| `matter`          | Task / todo service           | `${OCTO_MATTER_PORT}` (28086)     |
| `summary-api`     | Smart-summary HTTP API        | `${OCTO_SUMMARY_API_PORT}` (28087)|
| `summary-worker`  | Smart-summary background LLM  | (internal only)                   |

---

## Quick start

The default `docker-compose.yaml` references `octo-web:local`,
`octo-admin:local`, `octo-matter:local`, `octo-summary-api:local`, and
`octo-summary-worker:local`. Until the project publishes public registry
images, build them locally from the sibling OSS repos:

```bash
# First time only — build the frontend + side-service images.
# Clone the companion repos next to this one:
#   Mininglamp-OSS/octo-web
#   Mininglamp-OSS/octo-admin
#   Mininglamp-OSS/octo-matter
#   Mininglamp-OSS/octo-smart-summary
( cd ../octo-web          && docker build -t octo-web:local          . )
( cd ../octo-admin        && docker build -t octo-admin:local        . )
( cd ../octo-matter       && docker build -t octo-matter:local       . )
( cd ../octo-smart-summary && \
    docker build -f Dockerfile.api    -t octo-summary-api:local    . && \
    docker build -f Dockerfile.worker -t octo-summary-worker:local . )
```

Once those images exist:

```bash
cp docker/octo/.env.example docker/octo/.env
# edit docker/octo/.env — at minimum change the CHANGE_ME_* values
cd docker/octo
docker compose up -d
```

To use a registry-hosted image later, override via `.env` (e.g.
`OCTO_WEB_IMAGE=ghcr.io/your-org/octo-web:v1.0.0`).

Once healthy:

- Stack status:  <http://octo.local:28080/_octo_up>
- Web SPA:       <http://octo.local:28080/>
- Admin console: <http://octo.local:28080/admin/>
- REST ping:     <http://octo.local:28080/api/v1/ping>

Add `127.0.0.1 octo.local` to `/etc/hosts` on the browsing machine so the
host header routes correctly.

---

## Required `.env` values

These must be changed from their placeholders **before** first boot. Most
are checked at startup and the stack will refuse to run with defaults.

| Variable                     | Why                                                           |
| ---------------------------- | ------------------------------------------------------------- |
| `MYSQL_ROOT_PASSWORD`        | MySQL root — used by octo-server + init scripts.              |
| `MINIO_ROOT_PASSWORD`        | MinIO root user.                                              |
| `OCTO_MASTER_KEY`            | 32-byte key for at-rest credential encryption. `openssl rand -hex 16`. |
| `OCTO_NOTIFY_INTERNAL_TOKEN` | Shared secret between octo-server ↔ matter / summary.         |

---

## First-run: create the first admin

The stack boots with only system bots in the `user` table (`botfather`,
`notification`). You need to create a human administrator before the
`/admin/` console can be used.

```bash
# 1. Register a normal user. Requires mode=debug + non-empty smsCode in
#    configs/octo-server.yaml (the defaults are the OPPOSITE for safety).
#    Either relax them temporarily, or use the SQL method below.
curl -X POST "http://octo.local:28080/api/v1/user/register" \
  -H 'Content-Type: application/json' \
  -d '{"zone":"86","phone":"13800000001","code":"123456",
       "username":"admin","password":"CHANGE_ME",
       "name":"Admin"}'

# 2. Promote to admin.
docker exec -it octo-mysql \
  mysql -uroot -p"$MYSQL_ROOT_PASSWORD" octo \
  -e "UPDATE user SET role='admin' WHERE phone='13800000001';"

# 3. Log in at /admin/ using the phone-prefixed username (8613800000001)
#    and the password you chose above.
```

A future release will add an `octo-server create-admin` CLI and
`.env`-driven initial provisioning.

---

## 🚨 Production hardening

The default `configs/octo-server.yaml` in this directory is tuned for
**safe open-source distribution**:

- `mode: release` — skips debug-only shortcuts.
- `smsCode: ""` — no universal verification code.
- `register.off: true` — closed signups by default.

If you flip any of these back to `debug` / `"123456"` / `false` you are
running an IM server with open registration and a known bypass SMS code;
do not expose it to the public internet in that configuration.

Extra checklist before a public deployment:

1. Set `OCTO_WK_WSS_ADDR=wss://your.domain/ws` so WuKongIM advertises the
   canonical endpoint. Without it, `GET /route` returns `ws://127.0.0.1:5200/`
   and no browser will connect.
2. Terminate TLS at an outer nginx (or enable the stub 443 block in
   `nginx/conf.d/octo.conf.template`) and mount real certificates.
3. Wire up a real SMS provider under `smsProvider:` in
   `configs/octo-server.yaml` — replace the `smsCode` fallback.
4. Rotate MinIO + MySQL passwords, and regenerate the token shared with
   matter / summary (`OCTO_NOTIFY_INTERNAL_TOKEN`).
5. Disable `wukongim: tokenAuthOn: false` in shared environments.

---

## Tear down

```bash
cd docker/octo
docker compose down -v   # -v drops volumes (MySQL data, MinIO buckets…)
```
