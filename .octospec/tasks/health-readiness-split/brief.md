---
type: Task
title: "Task: health-readiness-split"
description: Split cheap liveness from bounded DB/Redis readiness checks.
tags: ["health", "readiness", "rate-limit"]
timestamp: 2026-06-23T00:00:00Z
slug: health-readiness-split
upstream: "Mininglamp-OSS/octo-server#436"
source: self
---

# Task: health-readiness-split

## Goal

Make `/v1/health` a cheap process liveness endpoint for frontend/LB probes, and
move DB/Redis dependency probing to a bounded readiness endpoint.

## Background

Issue #436: frontend status-tower polling `/v1/health` shows TTFB jitter because
the current handler synchronously runs global Redis-backed rate limiting,
MySQL `Ping()`, and Redis `Ping()`.

## Load-bearing list

- `/v1/health` response semantics change from dependency probe to process
  liveness.
- Add `/v1/ready` as the dependency readiness endpoint.
- Global Redis-backed rate limiting must not add Redis latency to the liveness
  probe path. Readiness intentionally keeps the global limiter because it
  touches DB/Redis dependencies.
- Dependency errors must be logged server-side and not exposed as raw response
  strings.

## Out of scope

- Background cached dependency probing.
- New Prometheus dependency health metrics.
- Deployment manifest changes for k8s probes.

## Acceptance

- `/v1/health` does not call DB or Redis and keeps the legacy success shape:
  `200 {"status":"up","db":"up","redis":"up"}`.
- `/v1/ready` checks DB/Redis with bounded timeout and returns `503` when any
  dependency is down.
- `/v1/ready` response includes safe `up/down` status fields only.
- `/v1/health` is excluded from the global Redis-backed IP limiter; `/v1/ready`
  keeps the global limiter because it touches DB/Redis dependencies.
- `/v1/health` and `/v1/ready` are excluded from high-volume access logging.
- Focused Go tests pass for `./modules/common`.
