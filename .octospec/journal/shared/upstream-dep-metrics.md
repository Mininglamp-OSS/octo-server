---
type: Journal
title: "Journal: upstream-dep-metrics (octo-server #440)"
description: Record of the P0-a upstream-dependency metrics (DB/Redis pool + object-storage latency) and the rules honored.
tags: ["observability", "metrics", "prometheus", "file-service"]
timestamp: 2026-06-23T09:23:17Z
# --- octospec extension fields ---
task: upstream-dep-metrics
upstream: Mininglamp-OSS/octo-server#440
source: self
---
# Journal: upstream-dep-metrics (octo-server #440)

## What was done
Made the dependencies below the HTTP handler observable (P0-a scope), so request
latency can be attributed instead of guessed. Two additions, both self-contained
in this repo (no `octo-lib` change, no handler/business-logic change):

1. **Dependency-latency histogram** — `pkg/metrics/dependency.go`:
   `dmwork_dependency_duration_seconds{dependency,op,backend,status}`, buckets
   down to 1ms. Exposed via a nil-safe package-level observer
   (`ObserveObjectStore`) so callers don't thread a metrics instance through.
   The `file.Service.DownloadURL` facade (`modules/file/service.go`) is wrapped
   transparently (return + error unchanged); `backend` label is captured in the
   `NewService` switch (minio/oss/qiniu/cos/s3/seaweedfs).

2. **Connection-pool metrics** — `pkg/metrics/pool.go`, scrape-time Collectors
   (no background goroutine):
   - DB via the canonical `collectors.NewDBStatsCollector(ctx.DB().DB,"main")` →
     standard `go_sql_*` (incl. `wait_count` / `wait_duration_seconds_total`).
   - Redis via a custom collector reading `rlRedis.PoolStats()` →
     `dmwork_redis_pool_*` (total/idle gauges; hits/misses/timeouts/stale counters).

Registered on `prometheus.DefaultRegisterer` in `main.go` right after `rlRedis`
is built, served by the existing `/metrics` scrape endpoint.

## Rules honored
- **error-handling** (load-bearing): `DownloadURL` wrapper propagates `(url, err)`
  unchanged; no user-facing error response added, no errcode/i18n touched.
  `make i18n-lint` green.
- **space-isolation / rate-limit** (load-bearing): no query/handler/throttle added;
  only reads `rlRedis.PoolStats()`. No isolation/limit surface touched.
- **testing**: 9 unit tests using `prometheus/testutil` + fresh registry; no
  MySQL/Redis/WuKongIM dependency. `go test -race ./pkg/metrics/... ./modules/file/...`
  green.

## Decisions / learnings
- **Scrape-time Collectors beat a background sampler.** #440 originally proposed a
  15s sampler goroutine; reading `.Stats()`/`.PoolStats()` on scrape is fresher and
  removes the goroutine-lifecycle/leak class entirely. Synced back to the issue.
- **DB pool → standard `go_sql_*`**, not hand-rolled `dmwork_db_pool_*`: the
  client_golang `DBStatsCollector` is less code and community-dashboard compatible.
- **lib redis pool is unreachable in P0-a**: `octo-lib`'s `GetRedisConn()` returns
  `*redis.Conn` (single-conn wrapper) which has no `PoolStats()`; only the
  repo-owned `*redis.Client` (rate-limit) is sampled. Lib coverage needs a lib change.
- go-redis **v6** has no `Hook` interface (v8+ only) → command-level timing stays
  out of scope (P0-b).

## Out of scope (P0-b, tracked on #440)
Per-query SQL timing (needs octo-lib `EventReceiver`), WuKongIM call timing (no
unified client), Redis command-level timing (v6 no hooks), lib redis pool stats.
