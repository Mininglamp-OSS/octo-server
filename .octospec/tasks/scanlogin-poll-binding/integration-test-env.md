---
type: Note
title: "Note: running octo-server integration tests locally"
description: Working setup for MySQL + Redis + WuKongIM, and the one non-obvious gotcha — the suite needs a fresh database per package, otherwise sql-migrate rejects the accumulated migration records.
tags: ["testing", "integration", "wukongim", "mysql", "redis"]
timestamp: 2026-08-07T00:00:00Z
task: scanlogin-poll-binding
---

# Running the integration tests locally

Verified on this branch, 2026-08-07. Without these three services, every test that
calls `testutil.NewTestServer()` panics at `dial tcp 127.0.0.1:3306: connect: connection
refused` — which reads like a broken test but is just a missing dependency.

## Services

```bash
# Redis
redis-server --daemonize yes --port 6379 --save "" --appendonly no

# MySQL 8.0
apt-get update && apt-get install -y mysql-server
mkdir -p /var/run/mysqld && chown mysql:mysql /var/run/mysqld
mysqld --user=mysql --daemonize
mysql -uroot <<'SQL'
ALTER USER 'root'@'localhost' IDENTIFIED WITH caching_sha2_password BY 'demo';
CREATE USER IF NOT EXISTS 'root'@'%' IDENTIFIED WITH caching_sha2_password BY 'demo';
GRANT ALL PRIVILEGES ON *.* TO 'root'@'%' WITH GRANT OPTION; FLUSH PRIVILEGES;
CREATE DATABASE IF NOT EXISTS test CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
SQL

# WuKongIM — note this is the release ASSET url, not the tag page
curl -fsSL -o /usr/local/bin/wukongim \
  https://github.com/WuKongIM/WuKongIM/releases/download/v2.2.4-20260313/wukongim-linux-amd64
chmod +x /usr/local/bin/wukongim
WK_MODE=debug WK_TOKENAUTHON=false WK_EXTERNAL_IP=127.0.0.1 \
  WK_EXTERNAL_WSADDR=ws://127.0.0.1:5200 wukongim -i &
# readiness: curl http://127.0.0.1:5001/health  =>  {"status":"ok"}
```

`WK_TOKENAUTHON=false` is the load-bearing one: the tests use octo-lib's default empty
manager token, so IM token auth has to be off or `/channel` and `/health` refuse the
connection. `WK_*` = `WK_` + the config key upcased (`tokenAuthOn` → `WK_TOKENAUTHON`).
Migrations are applied automatically by `testutil.NewTestServer` → `module.Setup`.

## Gotcha: one fresh database per package

**`go test ./...` does not work against a single `test` database, and `-p 1` does not fix
it.** Each module embeds its own migration set (`//go:embed sql`) and applies it via
sql-migrate into whatever database it is pointed at. Run two packages against the same
database and the second one finds `gorp_migrations` rows it does not recognise:

```
panic: Unable to create migration plan because of
       20191106000001_event_legacy01.sql: unknown migration in database
Error 1054 (42S22): Unknown column 'app_id' in 'field list'
```

Observed on this branch: `go test ./...` → 22 packages FAIL; with `-p 1` → the same 22
FAIL; each of those packages run **individually against a freshly created database** →
PASS. So the failures are an artifact of database reuse, not of the code.

Recreate the schema between packages:

```bash
for p in $(go list ./...); do
  mysql -uroot -pdemo -e "DROP DATABASE IF EXISTS test;
    CREATE DATABASE test CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;"
  redis-cli flushall > /dev/null
  go test "$p" -count=1
done
```

If you only need one module, a single `DROP`/`CREATE` beforehand is enough:

```bash
mysql -uroot -pdemo -e "DROP DATABASE IF EXISTS test;
  CREATE DATABASE test CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;"
go test ./modules/user/... -count=1
```

Redis also carries state the DB teardown does not clear — `CleanAllTables` does not touch
it, and the rate-limit buckets (`ratelimit:uid:*`) and now `scanlogin:poll:*` persist
across runs. `redis-cli flushall` between packages avoids surprises.
