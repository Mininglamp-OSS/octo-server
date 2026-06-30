---
type: Task
title: "Task: sticker-upload-handle"
description: Cryptographic upload handle binding a custom-sticker object to its uploader, closing the path-shape check's tail-match residual.
tags: ["sticker", "security", "wire-contract", "fullstack"]
timestamp: 2026-06-30T00:00:00Z
# --- octospec extension fields ---
slug: sticker-upload-handle
upstream: Mininglamp-OSS/octo-server#26
source: self
---

# Task: sticker-upload-handle

> Follow-up hardening on `custom-sticker-management` (octo-server #508 / octo-web
> #496). Closes the residual called out in that PR's review.

## Goal

Make custom-sticker registration (`POST /v1/sticker/user`) prove that the
client-supplied `path` was produced by THIS user's content-validated
`type=sticker` upload — not merely that the path *looks* like a sticker object.
`modules/file` signs `(uploaderUID, storedPath)` with an HMAC at upload time and
returns it as `sticker_handle`; `sticker.add` verifies it.

## Background

The shipped registration guard `sticker.validateStickerPath` is a pragmatic
object-key shape check: it accepts any path whose tail matches
`.../sticker/{uid}/<name>.<ext>`. Its documented residual (PR#508): a chat-bucket
object `chat/sticker/{uid}/x.gif` passes the tail match, so a 100MB `type=chat`
upload could be re-registered as a sticker, dodging the 1MB + raster-only
`type=sticker` upload contract.

A path string carries no proof of provenance, so the only robust fix is to bind
the object to its upload cryptographically. The HMAC key is derived from
`OCTO_MASTER_KEY` (the existing 32-byte boot secret used by `modules/common`
key-encryption) via one domain-separated HMAC pass, so the sticker-handle subkey
is independent of every other use of that master key.

## Load-bearing list

- **`modules/file` sticker upload contract** (touches: `wire-contract`) —
  `uploadFile` gains one response field, `sticker_handle`, emitted ONLY for
  `type=sticker` and ONLY when a master key is configured. No change to existing
  fields (`path`/`name`/`size`/`ext`/`sha512`) or to any non-sticker type.
- **`modules/sticker` registration guard** (touches: `auth`, `acl`,
  `wire-contract`) — `add` keeps `validateStickerPath` ALWAYS (defense in depth)
  and, when `stickersig.Enabled()`, additionally requires a valid handle. Both
  failure modes collapse to the single generic `request_invalid`/`path` code (no
  enumeration). When no master key is configured it degrades to the shape check
  alone — the pre-handle posture, so those deployments are not regressed.
- **`pkg/stickersig`** (new leaf package) — `Sign`/`Verify`/`Enabled`; HMAC-SHA256
  over length-prefixed fields, base64url, constant-time compare; no dependency on
  any `modules/*` package (so `modules/file` can use it without a cycle).
- **octo-web datasource** — `uploadSticker` surfaces `sticker_handle` as an
  optional `handle`; `addSticker` forwards it; `EmojiToolbar` threads it through.
  `handle` is optional end-to-end so a master-key-less backend still works.

## Out of scope

- Handle expiry / nonce / replay window — the handle authorizes registering one
  UUID-keyed object as the caller's own sticker; re-use is bounded by the quota
  and grants no capability the uploader lacks. No timestamp is signed.
- Pinning the storage host/origin — unchanged from #508; the handle makes host
  pinning unnecessary for provenance.
- Decode-time resolution/pixel-dimension cap (decompression-bomb defense) —
  tracked separately; the handle does not address it.
- Deriving the stored extension from magic bytes rather than filename — the
  upload already validates content against the declared ext via
  `ValidateMagicNumber`, and `validateStickerPath` pins `path-ext == format`.

## Acceptance

- `pkg/stickersig`: sign/verify round-trips; rejects tampered uid/path, malformed
  or empty handles, field-boundary collisions, and handles minted under a
  different master key; disabled (and Verify returns false) with no master key.
- `modules/file`: a `type=sticker` upload returns a `sticker_handle` that
  verifies for `(uploaderUID, returned path)` and not for a different uid; a
  non-sticker upload carries no handle.
- `modules/sticker` (integration, DB-backed): a shape-valid path is accepted
  WITH a valid handle and refused WITHOUT one or with a tampered one; the forged
  tail-match path `…/chat/sticker/{uid}/x.gif` passes the shape check yet is
  refused for lack of a handle; quota / ownership / concurrency behavior
  unchanged.
- `make i18n-extract-check` + `make i18n-lint` pass (no new codes added).
- octo-web: `uploadSticker` returns the handle (undefined when absent);
  `addSticker` forwards it verbatim; vitest green.
