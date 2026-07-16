---
type: Journal
title: "Journal: sticker-oversized-store-guard (fail-closed on oversized-uncompressed stickers)"
description: Fix the review-found regression where the compress-aware dimension gate admitted >512 jpg/png on the premise that compression downscales them, but every fail-open path (nil compressor, skipped on concurrency-saturation/timeout, failed, or compress_max_dimension > upload_max_dimension) stored the original oversized image up to 1024². Added a post-compression guard using the compressor's actual output dimension so a stored sticker never exceeds upload_max_dimension; deduped the cross-package 1024 constant.
tags: ["sticker", "wire-contract", "external-content", "throttle", "testing"]
timestamp: 2026-07-09T13:58:59Z
# --- octospec extension fields ---
task: sticker-oversized-store-guard
upstream: code-review of sticker-oversized-default
source: self
---

# Journal: sticker-oversized-store-guard

## What was done

An xhigh code review of `sticker-oversized-default` (5 independent finder agents,
strong convergence) surfaced a real regression I introduced: `effectiveGateDim`
widens the jpg/png dimension gate to 1024 when compression is on, trusting the
compressor to downscale to `compress_max_dimension`. But the compressor is
deliberately fail-open — so on `skipped:concurrency_saturated`, `skipped:timeout`,
`failed` (decode error), `f.compressor==nil`, or a `compress_max_dimension >
upload_max_dimension` mis-config, `uploadFile` stored the **original oversized
bytes** (up to 1024², 4× the 512 pixel budget) and served them inline to
conversation peers — the exact cross-user decode-DoS the dimension cap exists to
prevent, reachable under load and attackable by saturating the 4 compress slots.

Fix — enforce "stored dimension ≤ `upload_max_dimension`, on every path", without
giving up compression's *quality* fail-open:

- **`stickerCompressResult.OutMaxDim`** — the compressor now reports the actual
  post-compression single-edge dimension for `compressed`/`over_limit`
  (`doCompressStaticSticker` reads `img.Bounds()` after the optional Fit).
- **`api.go` store guard** — capture `stickerSrcMaxDim = max(cfg.W, cfg.H)` at the
  dimension gate; track `finalStoredMaxDim` (= `result.OutMaxDim` on the compressed
  path, else the source dim, the default for every store-original branch); after
  the compress block, `if finalStoredMaxDim > upload_max_dimension` →
  `observeStickerUpload("compress_oversized_rejected")` + reject. Placed AFTER the
  whole compress block so it also covers the nil-compressor / compress-off skip
  (which never enter the block).
- **New terminal metric** `compress_oversized_rejected` (registered + pre-warmed),
  distinct from the `compress_skipped/failed` sub-outcome so ops can see the guard
  firing during rollout.
- **Constant dedup** (finding 6) — exported `common.StickerUploadMaxDimensionHardCap`
  (alias of the unexported cap) and pointed `stickerCompressAcceptMaxDim` at it, so
  the compressible-accept ceiling and the decode hard cap are a single source of
  truth (a hand-synced 1024 literal could have drifted and re-widened the gate).
- **Schema note** — `compress_max_dimension` description now recommends
  `≤ upload_max_dimension` (else post-compress-oversized images are fail-closed).
- **Test cleanup** (finding 7) — `DownscaledAndStored` now uses the shared
  `uploadStickerForTest` helper instead of re-inlining the multipart boilerplate.

## Why this shape (design notes)

- **Guard, not gate-narrowing.** The gate SHOULD admit big jpg/png to shrink them;
  the bug is that admission was decoupled from a guaranteed ≤cap outcome. Checking
  the *actual output dimension* after the fact re-couples them robustly and covers
  the `compress_max_dimension > upload_max_dimension` mis-config too (compressed but
  not shrunk → OutMaxDim > cap → rejected), which a boolean "was compressed" flag
  would have missed.
- **Dimension fail-CLOSED, quality fail-OPEN.** Compression still never fails an
  upload for encode/timeout/saturation reasons *when the source already fits*
  (≤ upload_max_dimension → guard passes → stored original). Only an oversized
  source that couldn't be shrunk is rejected — the safe choice, since storing it
  is the harm.
- **Metric is a terminal label**, emitted in place of `success`; the
  `compress_skipped/failed` sub-outcome still records why compression didn't run,
  matching the existing sub+terminal counter design (a compressed success already
  emits both `compress_success` and `success`).

## Tests

`api_sticker_compress_test.go` — four guard regressions, each a 1024² jpg with
`upload_max_dimension=512`, asserting reject + nothing stored:
`OversizedGuard_NilCompressorRejects` (finding 5), `_CompressFailedRejects`
(injected failing `doCompress`), `_CompressTimeoutRejects` (injected slow
`doCompress` + 5 ms timeout → skipped:timeout, finding 1), and
`_CompressMaxDimAboveUploadDimRejects` (finding 3 mis-config → compressed-but-1024
→ rejected). Happy path (`OversizedJPEG_AcceptedAndDownscaled_DefaultUploadDim`,
1024²→512 stored) still green, proving the guard doesn't reject shrinkable images.
All no-infra file + common getter tests pass; `go vet` clean; both test binaries
compile; `make i18n-lint` unaffected. (Infra-only `TestGetAppConfig_*` /
`StickerCustomEnabled` panic on MySQL connect in this sandbox — pre-existing,
untouched by this change.)

## Learning

A "widen the gate because a later step will bring it back in bounds" change is
only as safe as that later step is guaranteed. Here the later step (compression)
was intentionally best-effort/fail-open, so the widen created a hole on every
fallback path. When you relax a bound trusting a downstream transform, assert the
post-transform invariant explicitly rather than trusting the transform to always
run — especially when the transform is fail-open by design.
