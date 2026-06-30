---
type: Task
title: "Task: sticker-handle-enforcement-rollout"
description: Decouple custom-sticker handle ENFORCEMENT (OCTO_STICKER_HANDLE_REQUIRED) from the signing CAPABILITY (OCTO_MASTER_KEY) so the #509 hardening ships as an observable, reversible gradual rollout instead of an implicit protocol flip.
tags: ["sticker", "security", "wire-contract", "observability", "config"]
timestamp: 2026-07-01T00:00:00Z
# --- octospec extension fields ---
slug: sticker-handle-enforcement-rollout
upstream: Mininglamp-OSS/octo-server#26
source: self
---

# Task: sticker-handle-enforcement-rollout

> Follow-up to `sticker-upload-handle` (octo-server #509). Keeps the security
> win of the signed upload handle but stops `OCTO_MASTER_KEY` from implicitly
> changing the new-sticker protocol the moment it is present.

## Goal

Split the sticker upload-handle **capability** from its **enforcement policy**:

- `stickersig.Enabled()` — server CAN mint/verify handles (i.e. a usable
  `OCTO_MASTER_KEY` is configured). Unchanged.
- `stickersig.Required()` — NEW policy switch `OCTO_STICKER_HANDLE_REQUIRED`
  (default `false`). Only when `true` does `POST /v1/sticker/user` reject a
  missing handle.

Because `OCTO_MASTER_KEY` is a mandatory production contract, tying enforcement
to `Enabled()` (as #509 did) silently flips the registration protocol and breaks
older clients that do not yet send a handle. This task makes enforcement an
observable, reversible rollout instead.

## Background

After #509, `sticker.add` rejected a missing/invalid handle whenever
`stickersig.Enabled()` was true. Since the master key is always present in
production, the "compatibility mode" the rollout needs never existed: every old
client without a handle started failing the moment the key was in place. The fix
is a dedicated policy flag, a client capability bit so clients know whether to
send a handle, and metrics so ops can confirm the missing-handle rate has
dropped to ~zero before flipping enforcement on.

## Load-bearing list

- **`POST /v1/sticker/user` registration guard** (touches: `security`,
  `wire-contract`) — `stickerPathTrusted` (bool) is replaced by
  `classifyStickerPath` (four-state). Decision matrix:
  - path-shape invalid → reject (always).
  - handle invalid/mismatched → reject (always, both modes).
  - handle missing + `Required()` → reject; + compat → allow + record.
  - ok → allow.
  Unknown classification is fail-closed (reject). **Compatibility-mode caveat:**
  with `required=false`, a missing handle is allowed, so the #509 cross-type
  bypass / foreign-object defense degrades to the path-shape check during the
  rollout window — the intended, reversible trade-off.
- **`/v1/file/upload?type=sticker` response** (touches: `wire-contract`) —
  unchanged shape; adds a `sticker_upload_handle_issued_total` metric on issue.
- **`GET /v1/common/appconfig` response** (touches: `wire-contract`) — adds
  `sticker_handle_required` (bool) in BOTH the version short-circuit and full
  branches, decoupled from `app_config.version` (same rationale as
  `local_login_off` / `search_enabled`).
- **Startup posture** (touches: `config`) — `required=true` with no usable
  master key logs a startup ERROR (not panic; a policy misconfig must not wedge
  service boot) and is surfaced on the `sticker_handle_policy` gauge.
- **`OCTO_STICKER_HANDLE_REQUIRED` env** (touches: `config`) — new deployment
  contract, default off (backward compatible), read live, parsed leniently.

## Out of scope

- No new error codes / no i18n locale changes — all rejections collapse to the
  existing `err.server.sticker.request_invalid` (anti-enumeration); the reason
  goes only to metrics/logs.
- No change to the legacy `c.ResponseError` calls in `modules/file` upload.
- No presigned-upload support for stickers (documented as unsupported).
- No change to the HMAC scheme, the path-shape check, or the decode-dimension
  cap shipped in #509.

## Acceptance

- `OCTO_STICKER_HANDLE_REQUIRED=false` (default): a shape-valid registration
  with no handle succeeds and increments `sticker_register_total{result=
  compat_missing}`; an invalid handle is still rejected.
- `OCTO_STICKER_HANDLE_REQUIRED=true`: missing / forged / mismatched handle all
  return `request_invalid`.
- Happy path: upload `type=sticker` returns `sticker_handle`; registering with
  that value as `handle` succeeds (`result=ok`).
- `GET /v1/common/appconfig` returns `sticker_handle_required` matching the env,
  in both the version-short-circuit and full branches.
- `required=true` with no/invalid master key produces a startup ERROR, not a
  panic, and registration is fail-closed.
- `go test ./pkg/stickersig/... ./pkg/metrics/... ./modules/sticker/...
  ./modules/common/... ./modules/file/...`, `make i18n-extract-check`,
  `make i18n-lint`, and `golangci-lint run` all pass.
