---
type: Journal
title: "Journal: sticker-oversized-default (>512 static jpg/png auto-shrinks to 512 by default)"
description: Make ">512px static jpg/png downscale to 512" the built-in default of the sticker compressor. compress_max_dimension default flips to 512 (decoupled from upload_max_dimension, clamped [1,1024]); the upload dimension gate becomes compress-aware — jpg/png accept up to the 1024 hard cap when compression is on (then shrink), gif/webp and compress-off stay gated at upload_max_dimension (512). compress_enabled stays default-false (gray-scale rollout preserved); zero-impact when off.
tags: ["sticker", "wire-contract", "external-content", "throttle", "testing"]
timestamp: 2026-07-09T12:58:13Z
# --- octospec extension fields ---
task: sticker-oversized-default
upstream: sticker-downscale-store (follow-up)
source: self
---

# Journal: sticker-oversized-default

## What was done

`sticker-downscale-store` made downscale *possible* but defaulted
`compress_max_dimension` to "= upload ceiling" (no shrink), so ">512 shrinks to
512" required hand-tuning three knobs. The product intent is simpler: **anything
over 512 should be compressed to 512**, and that should be automatic once
compression is enabled — without flipping compression on for every deployment
(the compressor keeps its designed gray-scale rollout).

Chosen approach (option B): keep `compress_enabled` default-false and
`upload_max_dimension` default-512 (so the appconfig client contract and the
compress-off gate are untouched), and instead:

- **`compress_max_dimension` default → 512**, clamp range `[1, 1024]` (the
  decoded-pixel hard cap), **decoupled** from `upload_max_dimension`. The getter
  collapsed to the shared `stickerClampIntUpper` helper like its siblings.
- **Compress-aware dimension gate** (`modules/file`): new
  `stickerLimitsSnapshot.effectiveGateDim(ext)` returns the compressible-accept
  ceiling `stickerCompressAcceptMaxDim` (1024) for jpg/png **when compression is
  on**, else `maxDim` (upload_max_dimension, 512). `api.go` uses it instead of
  the raw `maxDim`. So oversized static jpg/png are admitted to be shrunk;
  gif/webp (can't be re-encoded) and every upload when compression is off stay
  gated at 512.

The compute path (`doCompressStaticSticker` → `imaging.Fit` to
`compress_max_dimension`) is unchanged — this is the previously-measured
"ShrinkTo512" bench path (JPEG 1024²→512 ≈ 61 ms, PNG ≈ 94 ms, ≪ timeout).

## Why this shape

- **Chosen over "raise upload_max_dimension default to 1024".** That literal
  reading would ripple into the client contract: appconfig advertises
  `upload_max_dimension`, so it would jump to 1024 while the compress-off gate
  stayed 512 — an inconsistency (client told 1024, server rejects at 512). Making
  the gate compress-aware and leaving `upload_max_dimension` at 512 keeps
  appconfig honest and unchanged, and keeps compress-off byte-for-byte identical.
- **Compressible-accept ceiling fixed at the 1024 hard cap, not a new knob.** It
  is invisible in stored output (images are shrunk to `compress_max_dimension`
  anyway) and is the memory-safe maximum (1024²×4 = 4 MB decode/frame), so it
  needs no tuning.

## Invariants & envelope

- **Decompression-bomb / decode-memory bound unchanged.** The widened gate tops
  out at the SAME 1024 hard cap; gif/webp + compress-off stay at 512; the gate
  still fails closed on `DecodeConfig` error.
- **Wire contract unchanged.** Response shape, `ext`/`path` (jpg→jpg), handle,
  and appconfig `StickerUploadLimits.MaxDimension` (still `upload_max_dimension` =
  512) are untouched. `size`/signed bytes reflect the downscaled object as before.
- **Zero-impact when compression off (default).** `effectiveGateDim` returns
  `upload_max_dimension` (512) for every format, and the compressor never runs.
- **Known edge — APNG.** APNG has ext `.png` so it passes the widened gate, but
  the compressor detects it (`isAnimatedPNGSource`) and can't shrink it
  (`skipped:animated`). **Superseded by `sticker-oversized-store-guard`:** a
  >`upload_max_dimension` APNG is now fail-closed **rejected** (not stored), so the
  ≤`upload_max_dimension` stored invariant holds for animated PNGs too; a
  ≤`upload_max_dimension` APNG is stored as-is. (This entry originally said such an
  APNG was stored at full size — true only for the pre-guard increment.)

## Tests

- `system_settings_sticker_upload_test.go` — rewrote the 5 `compress_max_dimension`
  getter cases for the new semantics: default 512; decoupled from
  `upload_max_dimension`; ≤0/non-numeric → 512; in-range `[1,1024]` verbatim;
  `>1024` → 1024 + dedup Warn.
- `api_sticker_compress_test.go` — made `fakeStickerSystemSettings` faithful
  (unset → 512); added `OversizedJPEG_AcceptedAndDownscaled_DefaultUploadDim`
  (1024² jpg accepted with `upload_max_dimension=512`, stored 512²),
  `OversizedGIF_RejectedWhenCompressOn` (600² gif rejected at 512),
  `OversizedJPEG_RejectedWhenCompressOff` (zero-impact: 600² jpg rejected at 512
  when compression off), and repurposed the old unset-test into
  `DefaultCompressMaxDim_ShrinksOversizedTo512`. Reconciled
  `LargeJPEG_UsesCompressedBytes` (pinned `compressMaxDim=1024` to keep testing
  quality-only compression).
- Full file + common sticker suites pass; `go vet` clean; both test binaries
  compile; `make i18n-lint` unaffected; appconfig tests untouched.

## Learning

When a "default" request lands on a feature that was deliberately opt-in
(gray-scale), separate the two axes: the *config default* (what happens once the
feature is on) can be made "correct out of the box" without flipping the
*feature default* (whether it's on at all). Here that let us deliver "big images
auto-shrink" while preserving the compressor's cautious rollout — and, as a
bonus, avoided a client-contract change by making the gate compress-aware instead
of raising the advertised limit.
