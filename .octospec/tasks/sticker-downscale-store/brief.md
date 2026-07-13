---
type: Task
title: "Task: sticker-downscale-store"
description: Decouple the sticker compression downscale target from the upload dimension gate via a new sticker.compress_max_dimension setting, so static jpg/png larger than the target (but within the accept ceiling) are downscaled before re-encode+store instead of being stored at full size. Zero-impact default.
tags: ["sticker", "wire-contract", "external-content", "throttle"]
timestamp: 2026-07-09T12:30:28Z
# --- octospec extension fields ---
slug: sticker-downscale-store
upstream: TBD
source: self
---

# Task: sticker-downscale-store

> Phase-two follow-up on `sticker-upload-compression`. That task shipped the
> compressor with an `imaging.Fit` downscale step, but wired its target to the
> SAME value as the upload dimension gate (`upload_max_dimension`). Because the
> gate already rejects anything above that value, the downscale branch is
> unreachable on the production path — the compressor only ever re-encodes at the
> original size. This task decouples the two so "accept a larger static image,
> downscale it, then store" actually works.

## Goal

Add a new operator-tunable `system_setting` key **`sticker.compress_max_dimension`**
that is the target single-edge length the compressor downscales static `jpg/png`
INTO, independent of the `upload_max_dimension` accept gate.

- Semantics: unset / ≤0 → fall back to `StickerUploadMaxDimension()` (i.e. **no
  extra downscale → byte-for-byte identical to today**). Set → clamped to
  `[1, StickerUploadMaxDimension()]` (a target may only be **≤** the accept
  ceiling; a larger value is meaningless and is clamped down).
- Effect: with `upload_max_dimension=1024` + `compress_max_dimension=512` +
  `compress_enabled=true`, a static 1024² jpg/png is accepted at the gate,
  `imaging.Fit`-downscaled to fit 512×512 (aspect-ratio preserved), metadata
  stripped, re-encoded in its original format, and stored small.
- **The accept ceiling and the decompression-bomb gate are unchanged.** The
  `upload_max_dimension` hard cap stays **1024** (decided: keep it — no raising
  the accept ceiling, so full-decode memory stays ≤ 1024²×4 = 4 MB/frame). This
  task does NOT accept genuinely huge images; it only lets in-band static images
  between the target and the accept ceiling be shrunk.

## Background

`modules/file/sticker_compress.go:doCompressStaticSticker` already contains the
`imaging.Fit(img, maxDim, maxDim, Lanczos)` downscale. The dead-path root cause:
`stickerLimitsSnapshot.compressParams().MaxDim` returns `s.maxDim` — the very
value the dimension gate (`modules/file/api.go:426`) enforces — so after the gate
`w,h ≤ maxDim` always holds and `Fit` never fires. The fix is a one-value
decouple: give `compressParams()` a separate `compressMaxDim` sourced from the new
setting, leave the gate reading `maxDim`.

Config plumbing mirrors the sibling sticker keys exactly: canonical schema slice
entry (`system_setting_schema.go`), a clamp getter on `SystemSettings`
(`system_settings.go`), read through the 60s snapshot, surfaced in the
`stickerLimitsSnapshot` locked once per request (review F7 one-request-one-snapshot
invariant). The compress bench already measured this path — `perf-todo.md`
"ShrinkTo512" rows (JPEG 1024²→512 ≈ 61 ms, PNG 1024²→512 ≈ 94 ms on x86 Xeon),
well within `compress_timeout_ms`.

## Load-bearing list

- **`modules/file` sticker upload contract** (touches: `wire-contract`) — response
  field shape unchanged (`path`/`name`/`size`/`ext`/`sha512`/`sticker_handle`);
  when downscale runs, `size` and the signed bytes are the post-downscale values,
  exactly as post-compression today. No new request field. `ext`/`path` unchanged
  (jpg→jpg, png→png) so the `stickersig` handle and `validateStickerPath`
  `ext==format` invariant still hold.
- **Image decode / resource envelope** (touches: `external-content`, `throttle`) —
  downscale runs inside the existing compressor goroutine, bounded by the SAME
  timeout + concurrency semaphore; no new resource path. Full-decode memory is
  still bounded by the UNCHANGED accept gate (`upload_max_dimension` ≤ 1024), the
  decompression-bomb defense. This task must not weaken that gate or raise its
  hard cap.
- **`system_setting` schema + admin write path** (touches: `wire-contract`) — one
  new int key added to the canonical slice, `Positive:true`, hard-cap enforced by
  the read-side clamp getter (`≤ upload_max_dimension`). `custom_enabled` /
  appconfig client contract untouched (this key is server-side only; not exposed
  via appconfig — clients don't need the compression target for pre-check).
- **Zero-impact default** — with the key unset (every current deployment),
  `compressMaxDim == maxDim`, `Fit` still never fires, and behavior is identical
  to `main`.

## Out of scope

- Raising the `upload_max_dimension` accept hard cap (stays 1024). Accepting
  genuinely huge images (phone-screenshot scale) to downscale is explicitly NOT
  done here (would need a memory-budget review + concurrency retune).
- WebP / GIF / animated compression or downscale — still validate-only (phase-one
  scope unchanged); a non-compressible format cannot be downscaled.
- Exposing `compress_max_dimension` through `GET /v1/common/appconfig` — it's a
  server-side compression knob, not a client pre-check limit.
- Changing the upload→register contract, `stickersig`, quota, or handle rollout.
- Migrating `modules/file` to the i18n error envelope (no new error surface added
  by this task).

## Acceptance

- `sticker.compress_max_dimension` exists in the `system_setting_schema` slice
  (`Positive:true`, int), is accepted by the manager write path, and is read via a
  `SystemSettings.StickerCompressMaxDimension()` getter.
- Clamp getter unit-tested: unset → equals `StickerUploadMaxDimension()`; a value
  ≤0 or non-numeric → equals `StickerUploadMaxDimension()`; a value above the
  effective `upload_max_dimension` → clamped down to it; an in-range value is
  returned verbatim.
- With `compress_max_dimension` unset (default), upload is byte-for-byte identical
  to current `main` (regression: same stored bytes / `size` / `sticker_handle` for
  a static jpg that already fits).
- With `upload_max_dimension=1024`, `compress_enabled=true`,
  `compress_max_dimension=512`: a static 1024² jpg/png is stored with both edges
  ≤ 512 (aspect-ratio preserved), `size` reflects the downscaled bytes, and the
  `sticker_handle` verifies against the post-downscale stored object.
- A `gif`/`webp` upload is never downscaled (still `compress_skipped`); a 1024²
  gif under the accept gate is stored unchanged.
- The dimension gate still rejects anything above `upload_max_dimension`
  (`dimension_rejected` unchanged); the accept hard cap is still 1024.
- `go test ./modules/file/... ./modules/common/...` and `go vet` pass;
  `make i18n-lint` unaffected (no new i18n codes).
