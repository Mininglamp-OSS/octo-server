---
type: Task
title: "Task: webhook-grpc-hardening"
description: Harden the webhook gRPC callback listener — explicit opt-in auth mode that fails closed when chosen, constant-time token check, and explicit size bounds on callback payloads — without breaking deployments whose IM does not send a token.
tags: [webhook, trust-boundary, wire-contract, auth, test, testing, commit, security]
timestamp: 2026-09-28T00:00:00+08:00
# --- octospec extension fields ---
slug: webhook-grpc-hardening
upstream: none (internal hardening; report details intentionally omitted)
source: user
---

# Task: webhook-grpc-hardening

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. This repository is public: the brief, commits, PR title and PR body
> describe the change as configuration hardening only. Tests use synthetic
> values only.

## Goal

Make the webhook gRPC callback listener (`modules/webhook`) safe to operate by
configuration choice rather than by omission:

1. **Explicit auth mode.** Add an env switch `TS_GRPC_AUTH_REQUIRED`
   (default `false`).
   - `false` (default): current behavior is preserved — auth is enforced only
     when `TS_GRPC_AUTH_TOKEN` is non-empty. Existing deployments keep working.
   - `true` + empty token: `Start()` returns an error (process refuses to
     start). No listener is opened.
   - `true` + token set: every unary call must carry a matching `auth_token`.
   - Defense in depth: the auth interceptor itself rejects all calls if it is
     ever constructed with an empty expected token.
2. **Constant-time token comparison** in the auth interceptor.
3. **Payload bounds** on callback handling:
   - the compressed recipient list is decompressed through a size-limited
     reader; exceeding the limit is an error, not a truncation;
   - recipient lists are de-duplicated and capped in count before any lookup
     or push job is queued;
   - online-status entry lists are capped in length; malformed entries keep
     being skipped as today.
   Exceeding any limit rejects the whole event with an error — never a silent
   truncation. Limits are package constants with conservative defaults,
   documented next to the constant.
4. **Operator visibility.** At startup, log the effective auth mode: Warn when
   the listener is bound to a non-loopback address without auth, and Warn when
   a token is configured (the pinned IM does not send it, so its callbacks
   will be rejected); Info only for loopback without a token. Never log the
   token value.
5. **Docs.** State in every operator-facing doc that the default (`false`)
   keeps existing behavior, that `true` without a token (or an invalid value)
   makes the server refuse to start, and that WuKongIM `v2.2.4-20260313` does
   not send `auth_token`, so any configured token rejects IM callbacks:
   - `docs/webhook-grpc.md` (new; full behavior table, deployment advice,
     enablement order, startup logs, payload limits);
   - `configs/tsdd.yaml` (`grpcAddr` comment + security section);
   - `QUICKSTART.md` (Configure section);
   - `README.md` / `README.zh.md` (pointer next to the config template);
   - `docs/changelog.md` (`[Unreleased]` → 配置).

## Background

- `modules/webhook/api.go` `Start()` opens a gRPC listener on
  `config.GRPCAddr` (octo-lib default `0.0.0.0:6979`) and registers
  `WebhookService`. The auth interceptor is added only when
  `TS_GRPC_AUTH_TOKEN` is non-empty.
- The HTTP webhook path already refuses requests when its secret is not
  configured; the gRPC path does not have an equivalent explicit mode.
- WuKongIM releases through 2.2.x (checked: upstream tag `v2.2.4-20260313`)
  send no auth metadata on the webhook gRPC call and have no config field for
  one. Therefore enforcement must be opt-in in this repo; making it mandatory
  by default would stop all IM callbacks for existing deployments.
- The gRPC listener is separate from the HTTP API listener (`addr`, default
  `:8090`); restricting it does not affect client-facing traffic.

## Load-bearing list

- **wire-contract / webhook**: the `WebhookService.SendWebhook` contract with
  WuKongIM (event names `msg.notify`, `msg.offline`, `user.onlinestatus`;
  success/error status). Default-mode behavior must be byte-for-byte
  compatible for well-formed payloads within the new bounds.
- **auth**: `grpcAuthInterceptor` semantics — metadata key `auth_token`,
  `codes.Unauthenticated` on mismatch.
- **trust-boundary**: callback payloads are external input; bounds are applied
  at the handler entry, before DB writes, listener fan-out, or push queueing.
- **startup**: `Start()` returning an error aborts process start via
  octo-lib `module.Start`; this is the intended fail-closed path only in
  required mode.
- **env-registry**: the new env is not a credential and must NOT be added to
  `fixedInternalTokenEnvs` (`main.go`); `TS_GRPC_AUTH_TOKEN` stays there.

## Out of scope

- Changing the octo-lib default `GRPCAddr` (lives in octo-lib; a separate PR
  in that repo, not blocking this task).
- WuKongIM-side changes (adding a token config field and sending `auth_token`
  metadata; HTTP webhook signing). Separate task in the IM repo; this repo's
  required mode is designed to work once that ships.
- TLS / mTLS for the gRPC listener.
- Making auth mandatory by default (deferred until a WuKongIM release sends a
  token; then flip the default in a later task with release notes).
- Per-listener business re-validation of event fields (sender identity etc.) —
  separate design discussion.
- HTTP webhook routes (`/v1/webhook`, `/v1/webhook/message/notify`) and their
  existing HMAC behavior. (The payload bounds live in the shared event
  handlers, so they apply to both transports by design — trust-boundary
  adapter parity — but no HTTP routing or signing code changes.)
- Bounding the `msg.notify` message-list length (bounded today by the gRPC
  4 MiB default and the IM's per-push batch size; separate follow-up if
  needed).
- Deployment manifests in `octo-deployment`.

## Acceptance

Tests run without MySQL/Redis (start the listener on `127.0.0.1:0`-style free
ports with a minimal `config.Context`):

- [x] Default mode, token empty: `Start()` succeeds; an unauthenticated call
      with an unknown event returns `EventStatus_Success` (backward compat).
- [x] Default mode, token set: call without token → `codes.Unauthenticated`;
      call with correct token → success.
- [x] Required mode, token empty: `Start()` returns a non-nil error and no
      port is bound.
- [x] Required mode, token set: wrong token → `codes.Unauthenticated`;
      correct token → success.
- [x] Interceptor constructed with empty expected token rejects every call.
- [x] Token comparison uses `crypto/subtle.ConstantTimeCompare` (source
      assertion or review checklist).
- [x] Compressed recipient list whose decompressed size exceeds the limit →
      handler returns an error; nothing is queued.
- [x] Duplicate recipients are pushed at most once; recipient count (after
      de-duplication) above the cap → handler returns an error and nothing is
      queued (no truncation).
- [x] Online-status list above the cap → error; within cap → listeners
      receive the same parsed entries as before.
- [x] Startup log never contains the token value (test with a sentinel token).
- [x] `go vet ./modules/webhook/...`, `golangci-lint run ./modules/webhook/...`
      pass; `make i18n-lint` unchanged.
- [x] `go test ./modules/webhook/...` passes in full, and the full E2E suite
      (`ci/run-e2e-shard.sh 1 1`, 55 packages) passes locally against MySQL 8 /
      Redis 7 / WuKongIM `v2.2.4-20260313`.
- [x] CI unit suite (`ci/run-unit-tests.sh`, 52 packages) passes locally.
- [x] Integration check against WuKongIM `v2.2.4-20260313` with
      `WK_WEBHOOK_GRPCADDR` pointing at the octo-server listener:
      unset / `false` + no token → server starts, `msg.notify` rows persisted,
      no IM webhook errors; `true` + no token → server exits (exit code 2),
      port not bound; invalid value → server exits; `true` + token and
      token-only → server starts, IM callbacks rejected with
      `Unauthenticated`, nothing persisted.
- [x] Docs listed in Goal 5 state the default, the refuse-to-start case and
      the IM compatibility caveat.
- [x] Commit message describes configuration hardening only (no exploit or
      impact narrative). PR not opened yet; its title/body must follow the
      same rule.

### Review round 1 (PR #921, three approvals, P2 advisories adopted)

- [x] Duplicate-heavy recipient input is bounded before the full walk: the
      compressed list is decoded entry by entry and stops at
      `maxOfflineRecipientEntries` (2× the unique cap, before de-duplication);
      the plain list is checked against the same bound; the decompressed cap
      is lowered to 16 MiB to agree with the recipient cap.
- [x] Over-limit errors carry the observed input size; the rejection log
      carries `messageID` / `channelID` / `channelType` / `fromUID`.
- [x] A configured token logs Warn (IM compatibility note) instead of Info.
- [x] Token compare hashes both sides with SHA-256 before
      `subtle.ConstantTimeCompare` (no length side channel).
- [x] The auth interceptor runs before the language interceptor.
- [x] Docs updated (`docs/webhook-grpc.md`, `configs/tsdd.yaml`, changelog);
      tests added for each item.
- Not adopted here (follow-ups): `msg.notify` entry cap, HTTP webhook body
  size limit, typed gRPC status for over-limit events, TLS.
