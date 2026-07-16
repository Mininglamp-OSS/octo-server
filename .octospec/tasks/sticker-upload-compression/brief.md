---
type: Task
title: "Task: sticker-upload-compression"
description: Make custom-sticker upload limits (formats / max file size / max dimension) operator-configurable via system_setting with server-side hard caps, and add opt-in server-side compression for static jpg/png (decode → optional downscale → strip metadata → re-encode); webp/gif and animated images are validated-only in phase one.
tags: ["sticker", "wire-contract", "rate-limit", "fullstack"]
timestamp: 2026-07-07T06:54:12Z
# --- octospec extension fields ---
slug: sticker-upload-compression
upstream: TBD
source: self
---

# Task: sticker-upload-compression

> Follow-up on `custom-sticker-management` / `sticker-upload-handle`. Two coupled
> capabilities layered onto the same `modules/file` sticker upload path:
> (1) the previously hard-coded upload limits become operator-tunable with
> server-side hard caps; (2) an opt-in, phase-one server-side compressor for
> static raster stickers. Both default to today's behavior (zero-impact until an
> operator opts in).

## Goal

1. **Configurable upload limits.** The custom-sticker upload constraints —
   allowed formats, per-file size cap, max decoded dimension — currently
   hard-coded in `modules/file/const.go` (`stickerUploadExts`,
   `StickerMaxFileSize` = 1 MB, `StickerMaxDimension` = 512) become
   `system_setting` keys read through the existing 60s-snapshot `SystemSettings`
   singleton, so they can be greyed out / rolled back without a redeploy.
   Defaults reproduce today exactly. Every value is clamped by a **server-side
   hard cap** so a mis-config cannot open a resource-exhaustion hole:
   - max file size: default **1024 KB**, hard cap **5120 KB** (5 MB)
   - max dimension: default **512 px**, hard cap **1024 px**
   - allowed formats: default `gif,png,jpg,jpeg,webp`, intersected with the
     built-in raster allowlist (config may only **narrow**, never add non-raster
     types).

2. **Server-side compression (opt-in, phase one = plan C).** When enabled, a
   **static** `jpg/jpeg/png` upload is decoded → optionally downscaled to the
   configured target → stripped of metadata → re-encoded in its original format,
   before storage. `webp` and `gif` (and any animated image) are **validated
   only, never compressed** in phase one — because the current dependency set
   (`golang.org/x/image` webp = decoder-only; `disintegration/imaging` = no webp
   encoder) cannot re-encode webp without a new cgo dependency, which is
   deliberately out of scope. If a static image still exceeds the compression
   target after re-encode, the upload is **rejected** (not stored oversized).

3. **The `sticker_handle` is signed over the FINAL stored path/bytes.** Because
   compression may change the object (and, if a future phase changes the
   extension, the path), `stickersig.Sign` and the response `path`/`ext`/`size`
   MUST reflect the post-compression result. In phase one compression preserves
   the extension (jpg→jpg, png→png), so the path is unchanged; the invariant is
   still asserted so a later format-changing phase cannot silently break it.

4. **Ops & observability.** A default-OFF `compress_enabled` grey-out switch;
   compression bounded by configurable **timeout, concurrency, and memory**
   (decoded-pixel) ceilings, fail-open (skip compression, fall back to
   validate-then-store) so the compressor can never destabilize the main upload
   path; new low-cardinality metrics for compress success/failure/skip/over-limit
   alongside the existing upload-outcome counters.

## Background

`modules/file/api.go:uploadFile()` is the single choke point holding both the
authenticated uploader and the content-validated bytes; all sticker file-level
gating (size → ext allowlist → raster-only → magic number → path-ext match →
`image.DecodeConfig` dimension cap → store → `stickersig.Sign`) lives there, so
compression must slot into that same path — after the dimension cap (api.go
~397-428), before `service.UploadFile` (~463) and `stickersig.Sign` (~503).

Config plumbing reuses `modules/common/system_setting_schema.go` (canonical
schema slice; admin write path validates against it) + the `SystemSettings`
atomic-snapshot getters (60s auto-reload, already home to
`sticker.user_max_count / custom_enabled / handle_required`). There is **no
`featuregate` module in this repo**; grey-out is a `system_setting` bool. The
existing shared int bound is `[0, 3650]` (`settingIntMin/Max`); MB- and px-scale
caps need their own per-key clamp getters rather than that shared bound.

Metrics reuse `pkg/metrics/sticker.go` + `modules/file/sticker_metrics.go`
(`observeStickerUpload(result)` CounterVec, dimensions pre-warmed to 0).

Plan C was chosen (over transcoding webp→png/jpeg, or adding a cgo webp encoder)
to keep phase one pure-Go, zero new dependency, and low operational risk.

## Load-bearing list

- **`modules/file` sticker upload contract** (touches: `wire-contract`) — the
  upload response keeps its field shape (`path`/`name`/`size`/`ext`/`sha512`/
  `sticker_handle`); when compression runs, `size` and the signed bytes are the
  post-compression values. No change to any non-`type=sticker` upload.
- **`stickersig.Sign` provenance binding** (touches: `auth`, `acl`) — the handle
  must be minted over the final stored path (unchanged in phase one); the
  register-side `validateStickerPath` `ext==format` invariant must still hold.
  Reuses `sticker-upload-handle` guarantees; must not regress the
  `handle_required` fail-closed path or the `sticker/` keyspace reservation.
- **`system_setting` schema + admin write path** (touches: `wire-contract`) —
  new keys added to the canonical schema slice; the manager write path
  (`api_manager_system_setting.go`) and clamp getters enforce the per-key hard
  caps. `custom_enabled` / appconfig client contract untouched.
- **Upload-path resource envelope** (touches: `rate-limit`, `throttle`) —
  compression adds CPU/memory load inside the request; the concurrency semaphore
  + timeout + decoded-pixel ceiling are the throttle. Fail-open on saturation:
  no new hard failure mode vs. today. This is process-local compute throttling,
  NOT request-frequency limiting — the existing `SharedUIDRateLimiter` on the
  sticker routes and the file-module IP limiter are unchanged.
- **`modules/file` error-response convention** (touches: `error-response`) — the
  file module is NOT yet migrated to the i18n `httperr` envelope; new rejection
  paths use `c.ResponseError(...)` to stay consistent within the module (do not
  introduce a lone `httperr` call here).
- **Image decode surface** (touches: `trust-boundary`, `external-content`) —
  decode/re-encode operates on untrusted user bytes; decompression-bomb defense
  (decoded-pixel ceiling) and animated-image detection (gif via `DecodeAll`
  frame count, animated-webp via RIFF/VP8X `ANIM` chunk scan) are new pure-Go
  code that must fail safe.

## Out of scope

- **WebP / GIF / animated-image compression or transcoding.** Phase one is
  validate-only for these; no webp re-encoder, no cgo `libwebp`, no format
  conversion. (Explicitly deferred to a later phase.)
- **Changing the two-stage upload→register contract**, the `sticker` module
  register logic, quota (`user_max_count`), or the `handle_required` rollout.
- **Client / octo-web changes.** The upload API field shape is preserved; no new
  request field is required from clients.
- **Request-frequency rate limiting.** No change to `SharedUIDRateLimiter` or the
  file-module IP limiter; this task only adds compute-side throttling.
- **Migrating `modules/file` to the i18n error envelope.**
- **Async / out-of-band compression pipeline.** Phase one is synchronous within
  the upload request, bounded by timeout + concurrency.

## Acceptance

- `sticker.upload_max_size_kb`, `sticker.upload_max_dimension`,
  `sticker.upload_allowed_formats`, `sticker.compress_enabled`,
  `sticker.compress_target_kb`, `sticker.compress_max_concurrency`,
  `sticker.compress_timeout_ms` exist in the `system_setting_schema` slice, are
  accepted by the manager write path, and are read via `SystemSettings` getters.
- Clamp getters unit-tested: a configured value above the hard cap
  (size > 5120 KB, dimension > 1024 px) returns the hard cap (not the input) and
  logs a warning; `allowed_formats` is intersected with the raster allowlist
  (a config value of `gif,png,mp4` yields `gif,png` — never `mp4`).
- With `compress_enabled=false` (default), upload behavior is byte-for-byte
  identical to current `main` (regression test: same stored bytes, same
  `size`/`ext`/`sticker_handle`).
- With `compress_enabled=true`: a static jpg/png larger than the target is
  downscaled + re-encoded + metadata-stripped and stored **at or under** the
  target; a static jpg/png that still exceeds the target after re-encode is
  rejected with `observeStickerUpload("compress_over_limit")`.
- A `webp` or animated `gif`/`webp` upload is never re-encoded (metric
  `compress_skipped`), and is rejected iff it violates the (configurable) size /
  dimension / format limits — same as today.
- `sticker_handle` verifies against the **post-compression** stored bytes/path;
  register (`POST /v1/sticker/user`) with the returned `path`/`handle` succeeds,
  and `validateStickerPath`'s `ext==format` invariant holds.
- Compression is bounded: unit test asserts it aborts on the configured timeout,
  and that concurrency beyond `compress_max_concurrency` fails open (upload still
  succeeds via validate-then-store, metric `compress_skipped`).
- New metric dimensions `compress_success / compress_failed / compress_skipped /
  compress_over_limit` are registered and pre-warmed to 0.
- `go test ./modules/file/... ./modules/common/...` and `go vet` pass;
  `make i18n-lint` unaffected (no new i18n codes introduced).
