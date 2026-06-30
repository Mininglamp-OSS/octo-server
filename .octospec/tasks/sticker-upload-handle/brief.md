---
type: Task
title: "Task: sticker-upload-handle"
description: Harden custom-sticker uploads — a cryptographic upload handle binding the object to its uploader (closes the path-shape tail-match residual), plus a decode-time pixel-dimension cap (decompression-bomb defense).
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

Additionally, bound the **decoded** pixel dimensions of a sticker upload. The
1MB file-size cap limits compressed bytes but not decoded resolution, so a
small, highly-compressed image can decode to an enormous bitmap and OOM the
inline renderer — and because stickers are sent to peers, that is a cross-user
DoS. Cap each side at 512px, read header-only via `image.DecodeConfig`.

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
- **`sticker/` keyspace reservation** (touches: `auth`, `acl`) — both upload
  entry points (`uploadFile` and the presigned `getUploadCredentials`) reject a
  NON-`type=sticker` upload whose path lands in the `sticker/` object keyspace.
  Closes the cross-type overwrite (PR#509 review): on an OSS backend whose
  `BucketName` equals an upload-type prefix (e.g. `chat`), `ossNormalizeObjectKey`
  strips the leading `<bucket>/`, so `type=chat&path=/sticker/{uid}/x` would
  canonicalize onto a real sticker's object key and overwrite it with un-gated
  content while the already-minted handle (bound to the unchanged URL) still
  verifies — defeating the guard even in the keyed posture. Backend-agnostic; a
  no-op on backends that don't strip a prefix.
- **Upload-side sticker uid binding** (touches: `auth`, `acl`) — `uploadFile`
  requires a `type=sticker` upload's `path` to start with `/{loginUID}/`, so a
  user can only write into their OWN `sticker/{uid}/…` keyspace. Closes the
  cross-USER same-type overwrite (PR#509 review): registration
  (`validateStickerPath`) already pinned `uid==loginUID`, but the upload endpoint
  did not, so an authenticated peer who knows a victim's sticker object key could
  overwrite its bytes (the minted handle binds the URL, not the content, so the
  swap is invisible to the provenance check). Same bug class as the cross-type
  reservation above, for the cross-user case.
- **Sticker decode-dimension cap** (touches: `wire-contract`) — for
  `type=sticker`, after the magic-number check, `uploadFile` reads W×H via
  `image.DecodeConfig` (header-only, no full decode) and rejects either side >
  `StickerMaxDimension` (512). Decoders for gif/png/jpeg (stdlib) and webp
  (`golang.org/x/image/webp`, already a dep) are blank-imported so the registry
  covers every accepted sticker format. A file whose dimensions can't be read is
  rejected. The pointer is reset afterward so the upload copy is unaffected.
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
- Server-side transcoding/normalization/resizing of oversized stickers — they
  are rejected, not down-scaled (validate-and-store only, per the parent task).
- Animated-frame-count / total-pixel-budget limits beyond the per-side cap — the
  512² cap plus the 1MB byte cap bound worst-case memory; finer budgeting is not
  attempted here.
- Deriving the stored extension from magic bytes rather than filename — the
  upload already validates content against the declared ext via
  `ValidateMagicNumber`, and `validateStickerPath` pins `path-ext == format`.

## Deployment / rollout

**The handle guard activates implicitly when `OCTO_MASTER_KEY` becomes a valid
(exactly-32-byte) key — and that key is SHARED with `modules/common`'s IM
private-key encryption.** So a deployment that sets the key for encryption (its
primary purpose) also turns on sticker-handle enforcement: from that point
`sticker.add` REQUIRES a valid `handle`, and any caller that omits it (an
`addStickerReq` with `handle==""`) is refused (`stickersig.Verify` returns false
on an empty handle). There is no separate enable flag — enforcement is coupled to
key presence by design (keeps the surface minimal; the feature has no released
clients yet).

Consequence and required rollout order:

1. **Ship the client first.** octo-web must forward `sticker_handle` (upload
   response) as `handle` (registration request) BEFORE the server runs with a
   valid `OCTO_MASTER_KEY`. The companion octo-web PR (#496) must land together
   with / ahead of enabling the key, or sticker registration breaks for every
   client.
2. **Key-less / wrong-length deployments are not regressed.** With no key (or a
   non-32-byte key) `stickersig.Enabled()` is false and `add` degrades to the
   path-shape check alone — the pre-handle posture. `sticker.New` emits a one-time
   startup WARN in this state so the degraded posture is visible to operators.
3. **No decoupling toggle was added.** A future `OCTO_STICKER_HANDLE_REQUIRED`
   switch (enforce independently of key presence) was considered and deferred —
   it is only worth adding if a deployment needs the key for encryption while
   intentionally keeping sticker enforcement off; revisit if that scenario
   appears.

## Acceptance

- `pkg/stickersig`: sign/verify round-trips; rejects tampered uid/path, malformed
  or empty handles, field-boundary collisions, and handles minted under a
  different master key; disabled (and Verify returns false) with no master key.
- `modules/file`: a `type=sticker` upload returns a `sticker_handle` that
  verifies for `(uploaderUID, returned path)` and not for a different uid; a
  non-sticker upload carries no handle.
- `modules/file`: a `type=sticker` upload whose decoded dimensions exceed 512 on
  either side is rejected (tested at 513×513, 600×10, 10×600); exactly 512×512 is
  accepted; a real webp decodes and is accepted (proving webp registration), so
  the cap never silently rejects a valid webp.
- `modules/sticker` (integration, DB-backed): a shape-valid path is accepted
  WITH a valid handle and refused WITHOUT one or with a tampered one; the forged
  tail-match path `…/chat/sticker/{uid}/x.gif` passes the shape check yet is
  refused for lack of a handle; quota / ownership / concurrency behavior
  unchanged.
- `make i18n-extract-check` + `make i18n-lint` pass (no new codes added).
- octo-web: `uploadSticker` returns the handle (undefined when absent);
  `addSticker` forwards it verbatim; vitest green.
