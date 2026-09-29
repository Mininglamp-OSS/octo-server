---
type: Journal
title: "Journal: webhook-grpc-hardening"
description: The webhook gRPC callback listener gains an explicit opt-in auth mode (TS_GRPC_AUTH_REQUIRED, default false) that refuses to start without a token, a constant-time token check, structural bounds on callback payloads, and operator docs — without changing behavior for deployments whose IM does not send a token.
tags: ["webhook", "trust-boundary", "wire-contract", "auth", "config", "test"]
timestamp: 2026-09-29T00:00:00Z
# --- octospec extension fields ---
task: webhook-grpc-hardening
upstream: none
source: user
---
# Journal: webhook-grpc-hardening

## What was done

- `modules/webhook/grpc_auth.go` (new): `loadGRPCAuthConfig` parses
  `TS_GRPC_AUTH_TOKEN` / `TS_GRPC_AUTH_REQUIRED`; `true` without a token, or a
  value that is not a boolean, is a startup error. The interceptor compares
  with `crypto/subtle`, rejects every call when built with an empty token, and
  returns one generic `Unauthenticated` for every failure. `isLoopbackListenAddr`
  drives the startup log level.
- `modules/webhook/api.go`: `Start()` resolves the config before creating the
  server or binding the port; logs the effective mode (Warn only for a
  non-loopback address without a token; the token value is never logged).
  `Stop()` tolerates a server that was never created.
- `modules/webhook/event_limits.go` (new): decompressed-size cap on the gzip
  recipient list (`io.LimitReader`), de-duplication + count cap on offline
  recipients (early exit keeps the set bounded), entry cap on online-status
  lists. Over-limit events are rejected whole; nothing is truncated.
- Docs: new `docs/webhook-grpc.md`, plus `configs/tsdd.yaml`, `QUICKSTART.md`,
  `README.md`, `README.zh.md`, `docs/changelog.md`.

## Load-bearing decisions

- **Opt-in, not default.** WuKongIM `v2.2.4-20260313` sends no `auth_token`
  and has no config field for one (checked in its webhook client). Making auth
  mandatory by default would stop every IM callback, so the new switch
  defaults to `false` and existing behavior is unchanged.
- **Fail closed only when chosen.** `TS_GRPC_AUTH_REQUIRED=true` without a
  token refuses to start before any listener is opened, so the process never
  runs in a state the operator explicitly ruled out.
- **Caps sized from the peer's batching.** The IM sends every offline member of
  a channel in one `msg.offline` event and the whole online-status backlog in
  one `user.onlinestatus` event, so the caps are generous structural bounds,
  not traffic shaping.
- **Parity.** The caps live in the shared event handlers, so the HTTP webhook
  path gets the same bounds as gRPC (trust-boundary rule).

## Verification

- New tests (`grpc_hardening_test.go`, no DB) with `-race`; `ci/run-unit-tests.sh`
  (52 packages); full E2E suite `ci/run-e2e-shard.sh 1 1` (55 packages) against
  MySQL 8 / Redis 7 / WuKongIM `v2.2.4-20260313`, all passing.
- Integration check with WuKongIM's webhook pointed at the octo-server
  listener: default and `false` → callbacks persisted; `true` without token and
  an invalid value → process exits, port not bound; token set (with or without
  `true`) → IM callbacks rejected with `Unauthenticated`. The last row is the
  pre-existing token behavior, now documented.

## Follow-ups

- octo-lib: default `GRPCAddr` is `0.0.0.0:6979` (separate PR there).
- IM side: send `auth_token` from the webhook client, then flip the default.
- `msg.notify` list length is bounded only by transport and IM batch size.
