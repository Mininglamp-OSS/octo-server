---
type: Task
title: "Task: upstream-dep-metrics"
description: Expose upstream-dependency observability metrics (DB/Redis pool + object-storage latency), P0-a scope, self-contained in this repo.
tags: [observability, metrics, file-service, prometheus]
timestamp: 2026-06-23T09:05:49Z
# --- octospec extension fields ---
slug: upstream-dep-metrics
upstream: Mininglamp-OSS/octo-server#440
source: self
---

# Task: upstream-dep-metrics

## Goal
Make the dependencies *below* the HTTP handler observable, so request-latency can
be attributed. Today `pkg/metrics/http.go` measures the entrypoint, but DB/Redis
connection-pool saturation and object-storage call latency are a black box.

Deliver **P0-a only**: parts self-contained in this repo, no `octo-lib` change,
no business-logic change. Two additions:

1. **Connection-pool metrics** for MySQL and the repo-owned Redis client.
2. **Object-storage operation latency** for the `file.Service` operations that
   actually perform storage I/O — `UploadFile` (PutObject) and `GetFile`
   (GetObject+Stat). **NOT** `DownloadURL` (see Revision below).

> **Revision (post-review, #442 P1-1):** the first cut instrumented
> `Service.DownloadURL`, but every backend's `DownloadURL` is pure local URL
> string-building (no network I/O), so the histogram only measured sub-µs string
> work and was *misleading* ("objectstore latency <1ms"). Re-targeted to the
> facade methods that genuinely round-trip to storage: `UploadFile` and
> `GetFile` (`GetFile`'s `obj.Stat()` forces the request, avoiding minio's
> lazy-GetObject trap). `DownloadURL` is left un-instrumented. op label is now
> `upload_file` / `get_file`, not `download_url`.

## Background
- Upstream issue: Mininglamp-OSS/octo-server#440.
- HTTP metrics already register on `prometheus.DefaultRegisterer` and are served by
  the standalone `/metrics` scrape server (`pkg/metrics`, wired in `main.go:152-154`,
  gated by `DM_METRICS_ENABLED`).
- `ctx.DB().DB` is already used as a `*sql.DB` (`main.go:201`), so `.Stats()` is reachable.
- `file.Service` (`modules/file/service.go`) is the facade delegating to the
  backend `uploadService`; wrapping a method here covers all backends
  (minio/oss/qiniu/cos/s3/seaweedfs) in one place. Backend is chosen in the
  `NewService` switch. **Only `UploadFile`/`GetFile` do storage I/O**;
  `DownloadURL` is local URL assembly (see Revision).

### Design decisions (note: refine #440's wording)
- **Pool metrics use scrape-time custom Collectors, NOT a background sampler.**
  `#440` proposed a 15s sampler goroutine; a `prometheus.Collector` whose
  `Collect()` reads `.Stats()`/`.PoolStats()` on each scrape is strictly better —
  always fresh, **zero background goroutine, no leak risk**. This supersedes the
  issue's "sampler goroutine lifecycle" item.
- **DB pool uses the canonical `collectors.NewDBStatsCollector`** (ships with
  client_golang), exposing standard `go_sql_*` series (open/in_use/idle/
  wait_count/wait_duration_seconds_total). Preferred over hand-rolled `dmwork_db_pool_*`
  gauges: less code, community dashboards work out of the box. → metric names for
  DB will be `go_sql_*`, not `dmwork_db_pool_*` as the issue sketched.
- **Redis pool**: client_golang has no go-redis v6 collector, so a small custom
  Collector reads `rlRedis.PoolStats()` on scrape → `dmwork_redis_pool_*`.
- **Dependency latency**: one shared `dmwork_dependency_duration_seconds`
  HistogramVec, labels `{dependency, op, backend, status}`, extendable to future
  dependencies by adding label values rather than new metrics.

## Load-bearing list
- **`file.Service` facade contracts** (`UploadFile` / `GetFile` / `DownloadURL`):
  each timing wrapper MUST be transparent — same return values, same error
  propagated unchanged, no swallowing, no added latency beyond a `time.Since`.
  Used by the avatar path, upload handlers, and non-public-bucket reads. (path
  glob hits `error-handling`, `space-isolation`, `rate-limit` rules — confirm
  none are actually affected.)
- **`/metrics` registry contract**: register on `DefaultRegisterer` exactly once;
  `MustRegister` panics on dup. Must not collide with existing `dmwork_http_*` or
  oidc metrics. Naming follows existing `dmwork_` namespace (except DB → `go_sql_*`).
- **`main.go` startup wiring**: collectors registered alongside `httpMetrics`
  (`main.go:152`). No new goroutine, so no new shutdown path.
- **`rlRedis` reuse**: sample the existing rate-limit `*redis.Client`; do not open
  a new connection.
- **Label cardinality**: `dependency × op × backend × status` must stay bounded;
  no high-cardinality values (paths, ids, uids) in any label.

## Out of scope
- Per-query SQL timing (needs `octo-lib` to attach a dbr `EventReceiver`) — P0-b.
- WuKongIM call timing (no unified client; scattered `network.Post/Get`) — P0-b.
- Redis command-level timing (`go-redis v6` has no hook interface) — P0-b.
- Pool stats for `octo-lib`'s redis: `GetRedisConn()` returns `*redis.Conn`, which
  does not expose `PoolStats()`; unreachable without a lib change. Excluded.
- No change to `octo-lib`, no change to any handler/business logic, no new HTTP route.

## Acceptance
- [ ] `go_sql_*` series present at `/metrics` (DBStatsCollector registered on the
      MySQL `*sql.DB`).
- [ ] `dmwork_redis_pool_*` series present at `/metrics`, sourced from
      `rlRedis.PoolStats()` via a scrape-time Collector.
- [ ] `dmwork_dependency_duration_seconds{dependency="objectstore",op,backend,status}`
      observed on every `UploadFile` (op=`upload_file`) and `GetFile`
      (op=`get_file`) call, `status` distinguishing ok/error, buckets reaching
      1ms: `.001 .0025 .005 .01 .025 .05 .1 .25 .5 1 2.5 5`. `DownloadURL` is NOT
      instrumented (local URL build, no I/O — #442 P1-1).
- [ ] `UploadFile` / `GetFile` / `DownloadURL` behavior unchanged (return + error
      transparent); proven by test.
- [ ] No background goroutine introduced (collectors read on scrape).
- [ ] Unit tests: dependency wrapper records ok+error paths & preserves return;
      redis collector emits expected series (via `prometheus/testutil`); register
      twice on a fresh registry does not panic in normal startup.
- [ ] `go test ./pkg/metrics/... ./modules/file/...` green; `go vet` clean.
- [ ] `make i18n-extract-check` and `make i18n-lint` unaffected (no error codes touched).
- [ ] `go-reviewer` pass with no unresolved CRITICAL/HIGH.
