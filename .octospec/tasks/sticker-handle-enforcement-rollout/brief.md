---
type: Task
title: "Task: sticker-handle-enforcement-rollout"
description: Decouple custom-sticker handle ENFORCEMENT (system_setting sticker.handle_required) from the signing CAPABILITY (OCTO_MASTER_KEY) so the #509 hardening ships as an observable, reversible gradual rollout instead of an implicit protocol flip.
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
- `SystemSettings.StickerHandleRequired()` — NEW enforcement policy, the DB-backed
  `system_setting` key `sticker.handle_required` (default `false`). Only when
  `true` does `POST /v1/sticker/user` reject a missing handle.

Because `OCTO_MASTER_KEY` is a mandatory production contract, tying enforcement
to `Enabled()` (as #509 did) silently flips the registration protocol and breaks
older clients that do not yet send a handle. This task makes enforcement an
observable, reversible rollout instead. The policy lives in `system_setting`
(not an env var) so ops can toggle/roll it back from the admin console without a
redeploy — converging across replicas within the snapshot TTL.

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
  - handle missing + policy on → reject; + compat → allow + record.
  - policy on + no signing capability (no valid OCTO_MASTER_KEY) → reject
    (fail-closed, recorded as `rejected_no_capability`); intercepted in add()
    before classification so a missing capability never silently allows an
    enforced registration.
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
- **`system_setting sticker.handle_required`** (touches: `config`) — new DB-backed
  policy, default off (backward compatible), admin-toggleable, hot-reloaded via
  the SystemSettings snapshot (no env var, no restart).

## Out of scope

- No new error codes / no i18n locale changes — all rejections collapse to the
  existing `err.server.sticker.request_invalid` (anti-enumeration); the reason
  goes only to metrics/logs.
- No change to the legacy `c.ResponseError` calls in `modules/file` upload.
- No presigned-upload support for stickers (documented as unsupported).
- No change to the HMAC scheme, the path-shape check, or the decode-dimension
  cap shipped in #509.

## Acceptance

- `sticker.handle_required=false` (default): a shape-valid registration
  with no handle succeeds and increments `sticker_register_total{result=
  compat_missing}`; an invalid handle is still rejected.
- `sticker.handle_required=true`: missing / forged / mismatched handle all
  return `request_invalid`.
- Happy path: upload `type=sticker` returns `sticker_handle`; registering with
  that value as `handle` succeeds (`result=ok`).
- `GET /v1/common/appconfig` returns `sticker_handle_required` matching the setting,
  in both the version-short-circuit and full branches.
- `required=true` with no/invalid master key produces a startup ERROR (not a
  panic) AND rejects registration at the request path (fail-closed), recorded as
  `rejected_no_capability`. Pinned by TestSticker_RequiredWithoutCapability_FailsClosed.
- `go test ./pkg/stickersig/... ./pkg/metrics/... ./modules/sticker/...
  ./modules/common/... ./modules/file/...`, `make i18n-extract-check`,
  `make i18n-lint`, and `golangci-lint run` all pass.
