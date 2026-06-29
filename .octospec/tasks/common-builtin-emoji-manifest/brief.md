---
type: Task
title: "Task: common-builtin-emoji-manifest"
description: Server-side built-in emoji manifest endpoint so clients stop hardcoding the custom emoji list.
tags: [common, emoji, wire-contract]
timestamp: 2026-06-29T00:00:00Z
# --- octospec extension fields ---
slug: common-builtin-emoji-manifest
upstream: octo-web EmojiService hardcoding (internal request)
source: self
---

# Task: common-builtin-emoji-manifest

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

Add a public, cacheable `GET /v1/common/emojis` endpoint that returns the
built-in **custom** emoji manifest (`{ version, list:[{key, name, url}] }`) from a
server-side single source of truth, so web / iOS / Android / desktop clients can
fetch the list dynamically instead of each hardcoding it.

Today every built-in custom emoji (`[使命必达] / [崇尚行动] / [有品位] / [尚方宝剑]`)
is hardcoded in each client (token→image map + bundled PNG). Adding or renaming one
forces all clients to re-release. This endpoint moves the **list** (keys, labels,
order, version) to the server.

Scope of this task = **Option B, step 1**: the manifest JSON is embedded in the
server via `//go:embed`. The `url` field is **optional per item**:
- built-in emojis ship with `url: ""` — clients keep reusing their already-bundled
  PNG (looked up by the client's own fallback map), so no image hosting is needed
  in this step and the server binary stays small;
- any future emoji added on the server carries a non-empty `url` (object storage /
  file module), which clients render directly with **zero client re-release**.

The wire contract is identical to the eventual DB-backed Option A, so phase A is a
drop-in source swap behind the same endpoint.

## Background

- Mirror the freshly-merged precedent `GET /v1/group/avatar_palette` (#500,
  `modules/group/api.go:161` + `pkg/avatarrender/`): a **public** GET that exposes a
  server-side single source of truth with a content-based weak **ETag** +
  `Cache-Control: public, max-age=300, must-revalidate` + `If-None-Match`→`304`.
  Reuse `pkg/avatarrender.ETag` / `IfNoneMatch` (generic crc32 helpers, not avatar
  specific).
- Public (no auth), same rationale as the existing public reads in this module
  (`/v1/common/countries`, `/changelog`) and `avatar_palette`: the data is
  non-sensitive and is needed before login (emoji rendering on the login/preview
  surfaces). Routes go in the existing `commonNoAuth` group in
  `modules/common/api.go:77`.
- Success path uses `c.Response(...)` (the existing envelope helper, see
  `chatBgList`/`changelog`). There is **no error path** (data is in-memory after a
  one-time embed parse, exactly like `avatar_palette`), so no new `pkg/errcode`
  code and no i18n marker are required.
- `version` lives **inside** the embedded JSON; editing the manifest bumps it.

## Load-bearing list
<!-- tags mirror .octospec/rules/_index.yaml inject_when.touches -->
- **wire-contract**: new public JSON contract `{version, list:[{key,name,url}]}`.
  `key` is the message-body token `[xxx]` and MUST stay byte-identical to the
  current hardcoded tokens (old messages/old clients depend on it).
- **error-response / i18n**: confirm the new handler introduces **no** legacy/raw
  error response and needs no new error code (no error path). Must not regress the
  module's i18n posture.
- HTTP caching semantics (ETag / 304 / Cache-Control) — clients cache the manifest;
  a stale ETag must revalidate to the new list.
- `modules/common` route registration + module embed (`1module.go` `//go:embed`).

## Out of scope
- DB table, manager CRUD API, and file upload for emojis (that is Option A / a
  later phase). This step is embedded-JSON only.
- Hosting the built-in PNG images server-side (built-ins keep `url:""` and reuse the
  client bundle). Object-storage image hosting comes with Option A.
- Unicode emoji (the ~150 standard 😀 set) — those stay local on every client,
  untouched.
- Auth / rate-limit middleware (public read, same as sibling public reads;
  global per-IP floor already applies in `main.go`).

## Acceptance
- `GET /v1/common/emojis` returns 200 with body
  `{"version":<int>,"list":[{"key":"[使命必达]","name":"使命必达","url":""}, ...]}`
  containing exactly the current built-in custom set, in order.
- Response carries a weak `ETag` and `Cache-Control: public, max-age=300,
  must-revalidate`; a matching `If-None-Match` yields `304` with no body.
- Endpoint is reachable without an auth token.
- `go test ./modules/common/...` passes, including a new test asserting the list
  contents, the 304 path, and that every `key` is a `[xxx]` token.
- `golangci-lint run ./modules/common/...` clean; `make i18n-extract-check` and
  `make i18n-lint` still pass (no new codes expected).
- Swagger entry added in `modules/common/swagger/api.yaml`.
