---
type: Journal
title: "Journal: sticker-downscale-store (accept-large-then-downscale for static stickers)"
description: Record of the phase-two sticker compression change — decouple the compressor's imaging.Fit downscale target from the upload dimension gate via a new server-side system_setting sticker.compress_max_dimension, so static jpg/png larger than the target (but within the unchanged accept ceiling) are downscaled before re-encode+store instead of being stored at full size. Zero-impact default (unset == upload_max_dimension == no downscale). Accept hard cap stays 1024; webp/gif still validate-only.
tags: ["sticker", "wire-contract", "external-content", "throttle", "testing"]
timestamp: 2026-07-09T12:30:28Z
# --- octospec extension fields ---
task: sticker-downscale-store
upstream: sticker-upload-compression (phase two)
source: self
---

# Journal: sticker-downscale-store

## What was done

The `sticker-upload-compression` compressor already carried an
`imaging.Fit(img, maxDim, maxDim, Lanczos)` downscale step, but it was
**unreachable on the production path**: `stickerLimitsSnapshot.compressParams()`
sourced its `MaxDim` from `s.maxDim` — the exact value the upload dimension gate
(`api.go` decode-config check) enforces — so after the gate `w,h ≤ maxDim` always
held and `Fit` never fired. The compressor only ever re-encoded at the original
size. This task decouples the shrink target from the accept gate so "accept a
larger static image, downscale it, then store" actually works.

- **`modules/common/system_settings.go`** — new getter
  `StickerCompressMaxDimension()`. Semantics: unset/≤0 → falls back to
  `StickerUploadMaxDimension()` (no extra downscale; byte-for-byte identical to
  before); a value above the effective `upload_max_dimension` is meaningless
  (shrink target can't exceed the accept ceiling) → clamped DOWN to it with a
  dedup Warn (reuses the `stickerClampWarned` sync.Map, same as the sibling
  clamp getters); an in-range `[1, upload_max_dimension]` value is returned
  verbatim. No independent hard cap — the accept ceiling (itself hard-capped at
  1024) is the upper bound, so decoded-pixel memory is unchanged.
- **`modules/common/system_setting_schema.go`** — one new canonical key
  `sticker.compress_max_dimension` (`settingTypeInt`, `Positive:true`, so it opts
  out of the shared `[0,3650]` bound; read-side clamp enforces `≤
  upload_max_dimension`). NOT exposed via `GET /v1/common/appconfig` — it's a
  server-side compression knob, not a client pre-check limit.
- **`modules/file/sticker_compress.go`** — added `compressMaxDim` to
  `stickerLimitsSnapshot` (populated from the new getter in the production branch,
  and `= StickerMaxDimension` in the nil-settings fallback so old unit tests stay
  equivalent), added `StickerCompressMaxDimension()` to the `stickerSystemSettings`
  interface, and changed `compressParams().MaxDim` to return `compressMaxDim`
  instead of `maxDim`. That one-value swap is the whole behavior change:
  `doCompressStaticSticker` and the `api.go` dimension gate are untouched — the
  gate still reads `maxDim`, the compressor now Fits into the smaller
  `compressMaxDim`.

## Invariants preserved

- **Accept gate / decompression-bomb defense unchanged.** `upload_max_dimension`
  (hard cap 1024) still bounds the full-decode pixel buffer; the new setting only
  shrinks WITHIN that ceiling, so the memory envelope (≤ 1024²×4 = 4 MB/frame ×
  concurrency) is not widened. Decision (recorded in brief): keep the 1024 accept
  cap — this task does NOT accept genuinely huge images.
- **Wire contract.** Response shape unchanged; `size` + signed bytes are the
  post-downscale values (same mechanism as post-compression). `ext`/`path`
  unchanged (jpg→jpg, png→png) so the `stickersig` handle and
  `validateStickerPath` `ext==format` invariant still hold.
- **Zero-impact default.** Unset key → `compressMaxDim == maxDim` → `Fit` never
  fires → identical to `main`. Regression test asserts a 700² source is stored at
  700² when the key is unset.
- **webp/gif/animated still validate-only** — a non-compressible format cannot be
  downscaled (phase-one scope unchanged).

## Tests

- `system_settings_sticker_upload_test.go` — 5 no-infra getter cases: unset→
  ceiling, unset-tracks-raised-ceiling, non-positive→ceiling, in-range verbatim,
  above-ceiling→clamp-down + dedup Warn.
- `api_sticker_compress_test.go` — 2 api-level integration cases:
  `LargeImage_DownscaledAndStored` (1024² jpg, `upload_max_dimension=1024` +
  `compress_max_dimension=512` → stored 512×512, aspect preserved, `size` =
  post-downscale, handle verifies) and `UnsetCompressMaxDim_NoDownscale`
  (regression: 700² stored at 700² when the key is unset). Fake
  `fakeStickerSystemSettings` extended with `compressMaxDim` mirroring the
  production fallback.
- `go vet` clean; both full test binaries compile; `make i18n-lint` unaffected
  (no new codes). Perf: this is the previously-measured "ShrinkTo512" bench path
  (JPEG 1024²→512 ≈ 61 ms, PNG ≈ 94 ms on x86 Xeon), well within
  `compress_timeout_ms`.

## Learning

The dead `imaging.Fit` branch is a good reminder that a "configurable" knob wired
to the same source as its own guard is silently inert. When a validation gate and
a downstream transform read the same limit, the transform can be unreachable —
worth a decoupling check whenever a resize/normalize step sits behind a reject
gate keyed on the same dimension.
