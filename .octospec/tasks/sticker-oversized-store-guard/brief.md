---
type: Task
title: "Task: sticker-oversized-store-guard"
description: Fix the review-found regression where the compress-aware dimension gate admits >512 jpg/png on the assumption compression downscales them, but every path where compression does NOT actually shrink (nil compressor, skipped on concurrency-saturation/timeout, failed fail-open, or compress_max_dimension > upload_max_dimension) stored the original oversized image. Add a fail-closed post-compression guard so a stored sticker never exceeds upload_max_dimension.
tags: ["sticker", "wire-contract", "external-content", "throttle"]
timestamp: 2026-07-09T13:58:59Z
# --- octospec extension fields ---
slug: sticker-oversized-store-guard
upstream: code-review of sticker-oversized-default
source: self
---

# Task: sticker-oversized-store-guard

> Fix for the top code-review finding on `sticker-oversized-default`. The
> compress-aware gate (`effectiveGateDim`) widens the jpg/png dimension gate to
> 1024 when compression is on, on the premise that the compressor downscales to
> `compress_max_dimension`. But the compressor is deliberately fail-open, so on
> concurrency-saturation / timeout / decode-failure — and when `f.compressor`
> is nil, or `compress_max_dimension > upload_max_dimension` — the original
> oversized bytes were stored and served to conversation peers, up to 1024²
> (4× the 512 pixel budget). Old behavior rejected anything >512 outright, so
> this was a regression in the cross-user decode-DoS protection, reachable under
> load and deliberately attackable by saturating the compress slots.

## Goal

Enforce the invariant **stored sticker dimension ≤ `upload_max_dimension`, on
every path**, without giving up compression's quality fail-open:

- The compressor reports the actual output dimension (`OutMaxDim`) for the
  `compressed`/`over_limit` outcomes.
- `uploadFile` records the source dimension at the gate and, after the compress
  block, computes `finalStoredMaxDim` (= compressed output dim on the compressed
  path, else the source dim) and **rejects** (`compress_oversized_rejected`) if it
  exceeds `upload_max_dimension`.
- Dimension stays fail-CLOSED even though compression quality stays fail-open: an
  oversized upload that could not be shrunk is rejected, never stored big.

Also dedup the two `1024` literals (finding 6): export the common hard cap and
reference it from `modules/file` so the compressible-accept ceiling can't drift.

## Load-bearing list

- **Decode-DoS / dimension bound** (touches: `external-content`) — restores the
  guarantee that a stored/served sticker is ≤ `upload_max_dimension` for every
  format and every compression outcome. The accept gate still admits jpg/png up
  to the 1024 hard cap (bounded decode memory), but nothing >`upload_max_dimension`
  is stored.
- **`modules/file` upload contract** (touches: `wire-contract`) — happy path
  unchanged (a 1024² jpg still accepted + shrunk to 512 + stored + handle-signed);
  only the fail-open-oversized paths now reject instead of storing. New terminal
  metric label `compress_oversized_rejected` (pre-warmed).
- **Resource envelope** (touches: `throttle`) — the guard is a pure dimension
  comparison after the existing compress block; no new compute or I/O.
- **Cross-package constant** — `stickerCompressAcceptMaxDim` now references
  `common.StickerUploadMaxDimensionHardCap` (single source of truth).

## Out of scope

- Changing `effectiveGateDim`'s widening (the gate correctly admits jpg/png up to
  1024 to shrink them; the fix is the post-store guard, not narrowing the gate).
- Clamping `compress_max_dimension ≤ upload_max_dimension` in the getter (kept
  decoupled; the mis-config is instead caught fail-closed by the guard and flagged
  in the schema description).
- appconfig field shape / advertised value (still `upload_max_dimension`; now
  honest since stored ≤ it).
- WebP/GIF/animated handling (unchanged; they were never widened).

## Acceptance

- With `compress_enabled=true`, `upload_max_dimension=512`: a 1024² jpg/png is
  rejected with `compress_oversized_rejected` (not stored) when compression does
  not shrink it — covered for: `f.compressor==nil`; injected `failed`; injected
  `skipped:timeout`; and `compress_max_dimension=1024 > upload_max_dimension=512`.
- The happy path is unbroken: a 1024² jpg with `compress_max_dimension=512` is
  still accepted, downscaled to 512×512, stored, and handle-verified.
- gif/webp and compress-off paths unchanged (source ≤ gate ≤ upload_max_dimension,
  guard never fires).
- `compress_oversized_rejected` label is registered and pre-warmed to 0.
- `stickerCompressAcceptMaxDim == common.StickerUploadMaxDimensionHardCap`.
- `go test ./modules/file/... ./modules/common/...` (no-infra subset) + `go vet`
  pass; `make i18n-lint` unaffected.
