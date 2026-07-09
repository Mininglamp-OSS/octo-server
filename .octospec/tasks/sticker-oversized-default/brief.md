---
type: Task
title: "Task: sticker-oversized-default"
description: Make ">512px static jpg/png auto-downscale to 512" the built-in default of the sticker compressor — set compress_max_dimension default to 512 and make the upload dimension gate compress-aware (jpg/png accept up to the 1024 hard cap when compression is on, then shrink; gif/webp and compress-off stay gated at upload_max_dimension). compress_enabled stays default-false (gray-scale rollout preserved); zero-impact when off.
tags: ["sticker", "wire-contract", "external-content", "throttle"]
timestamp: 2026-07-09T12:58:13Z
# --- octospec extension fields ---
slug: sticker-oversized-default
upstream: sticker-downscale-store (phase two follow-up)
source: self
---

# Task: sticker-oversized-default

> Follow-up on `sticker-downscale-store`. That task added
> `compress_max_dimension` but defaulted it to "= upload ceiling" (no downscale),
> so an operator had to hand-tune three knobs to get ">512 shrinks to 512". The
> product intent is simpler: **anything over 512 should be compressed (shrunk to
> 512)** — and that should be the default behavior of the compressor, not a
> three-knob opt-in. `compress_enabled` itself stays default-false so the
> compressor keeps its designed gray-scale rollout (perf-todo playbook).

## Goal

When an operator enables compression (`compress_enabled=true`, the single
gray-scale switch), a static jpg/png larger than 512px is downscaled to 512 and
stored — with no further per-deploy tuning. Two changes:

1. **`compress_max_dimension` default → 512** (was "= upload ceiling"). Clamp
   range becomes `[1, 1024]` (the decoded-pixel hard cap), decoupled from
   `upload_max_dimension`. It is the jpg/png shrink target.

2. **Compress-aware dimension gate.** The gate's effective max is:
   - jpg/png **and** `compress_enabled` → the compressible-accept ceiling **1024**
     (= the existing dimension hard cap), so oversized static images are admitted
     to be shrunk;
   - otherwise (gif/webp/animated, or compression off) → `upload_max_dimension`
     (default 512), unchanged.

   So gif/webp (which cannot be re-encoded/shrunk in phase C) and every upload
   when compression is off stay gated at 512 exactly as today.

**`upload_max_dimension`, `compress_enabled`, and the appconfig client contract
are unchanged.** `upload_max_dimension` (default 512) remains the advertised
limit, the gif/webp gate, and the compress-off gate. This keeps the appconfig
`StickerUploadLimits.MaxDimension` at 512 (no client-contract change) and keeps
compress-off byte-for-byte identical to `main`.

## Background

`sticker-downscale-store` already routes `compressParams().MaxDim` from
`compressMaxDim`; the compressor's `imaging.Fit` shrinks to it. The only reason
">512 shrinks to 512" was not automatic: `compress_max_dimension` defaulted to
the accept ceiling (so no shrink) and the gate rejected >512 before the
compressor could see it. This task flips the default and widens the gate for
compressible formats only — the compute path is unchanged (the "ShrinkTo512"
bench rows already measured it: JPEG 1024²→512 ≈ 61 ms, PNG ≈ 94 ms, ≪ timeout).

The compressible-accept ceiling is fixed at the dimension hard cap (1024) rather
than a new operator knob: it is invisible in stored output (images are shrunk to
`compress_max_dimension` anyway) and is the memory-safe maximum (1024²×4 = 4 MB
decode/frame), so it needs no tuning.

## Load-bearing list

- **`modules/file` sticker upload contract** (touches: `wire-contract`) — response
  shape unchanged; `size`/signed bytes are post-downscale (as today when
  compression runs). `ext`/`path` unchanged (jpg→jpg, png→png) so `stickersig`
  handle + `validateStickerPath` `ext==format` hold. appconfig
  `StickerUploadLimits.MaxDimension` stays `upload_max_dimension` (512) — no
  client-facing change.
- **Dimension gate / decompression-bomb defense** (touches: `external-content`) —
  the gate now admits jpg/png up to 1024 when compression is on, but 1024 is the
  UNCHANGED decoded-pixel hard cap, so the decode-memory envelope (≤ 1024²×4 =
  4 MB/frame) is not widened. gif/webp and compress-off stay at 512. The gate is
  still the only decode-dimension bound; it must fail closed on decode error.
- **Resource envelope** (touches: `throttle`) — downscale runs in the existing
  compressor goroutine under the unchanged timeout + concurrency semaphore; no
  new resource path.
- **Zero-impact when compression off** — with `compress_enabled=false` (default),
  the gate is `upload_max_dimension` (512) for every format and the compressor
  never runs, so behavior is identical to `main`.

## Out of scope

- Changing `compress_enabled`'s default (stays false — gray-scale rollout
  preserved) or `upload_max_dimension`'s default (stays 512).
- Making the compressible-accept ceiling an operator knob (fixed at the 1024 hard
  cap).
- WebP / GIF / animated downscale (still validate-only; a non-compressible format
  cannot be shrunk, so >512 of those is rejected at the 512 gate, not stored big).
- Any appconfig field-shape or advertised-value change.
- Async/out-of-band compression.

## Acceptance

- `StickerCompressMaxDimension()` unset → 512; a value > 1024 clamps to 1024
  (with dedup Warn); an in-range `[1,1024]` value is verbatim; value ≤0/non-numeric
  → 512. (Decoupled from `upload_max_dimension`.)
- With `compress_enabled=true`, `upload_max_dimension=512` (default),
  `compress_max_dimension` unset: a static **1024² jpg/png is accepted** (not
  rejected at 512) and **stored at 512×512** (aspect preserved); `size`/handle
  reflect the downscaled object.
- With `compress_enabled=true`: a **>512 gif is rejected** at the 512 gate
  (`dimension_rejected`) — not stored oversized.
- With `compress_enabled=false` (default): a **>512 jpg is rejected** at the 512
  gate (compress-off path unchanged) — zero-impact regression proof.
- Existing sticker upload/compress tests still pass unchanged.
- `go test ./modules/file/... ./modules/common/...` and `go vet` pass;
  appconfig tests unaffected; `make i18n-lint` unaffected.
