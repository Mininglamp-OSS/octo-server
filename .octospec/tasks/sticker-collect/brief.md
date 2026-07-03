---
type: Task
title: "Task: sticker-collect"
description: Add a server-side endpoint for collecting sticker-message paths into a user's custom sticker list.
tags: ["sticker", "api", "database", "quota"]
timestamp: 2026-07-03T00:00:00Z
# --- octospec extension fields ---
slug: sticker-collect
upstream: "manual"
source: self
---

# Task: sticker-collect

## Goal
Add `POST /v1/sticker/user/collect` so a client can add a sticker from an
existing sticker message into the caller's personal sticker list without
re-uploading the file.

## Background
The existing `POST /v1/sticker/user` endpoint is intentionally upload-oriented:
it only accepts the caller's own `sticker/{uid}/...` path and may require an
upload handle. Sticker-message collection has a different source: a sticker path
already present in a message payload, often under another user's UID segment.

This first version stores a reference to the source sticker object instead of
copying bytes into the collecting user's directory. The current file service
does not expose a storage-backend-neutral object-copy abstraction across
MinIO/S3/COS/OSS/Qiniu.

## Load-bearing list
- Authenticated `/v1/sticker` routes keep `AuthMiddleware` and shared UID rate limiting.
- Per-user quota still uses `sticker.user_max_count` and the existing user-row lock.
- Repeated collection of the same source path by the same UID is idempotent.
- Only reserved `sticker/{source_uid}/{file}.{gif|png|jpg|jpeg|webp}` paths are accepted.
- `sticker.handle_required` remains an upload-registration policy for `POST /v1/sticker/user`; collect intentionally has no handle parameter.

## Out of scope
- Verifying that the caller can read the exact source message that carried the sticker path.
- Server-side object copy or reference counting.
- Object existence HEAD/check before insert.
- Garbage collection semantics for collected references.

## Known Trade-Offs
- Because collect stores a reference, future object-store GC or source-user cleanup could create dangling collected stickers unless reference counting or copy-on-collect is added.
- Source sticker deletion currently soft-deletes only DB rows and does not delete the object; collected references depend on that retention behavior.
- Path-only source validation does not prove message access. A follow-up API can accept message identity and derive the sticker path server-side.

## Acceptance
- Same source path collected twice by the same user returns the existing sticker and does not consume quota twice.
- Soft-deleting a collected sticker releases the live idempotency slot and allows re-collecting it.
- Invalid/non-sticker paths are rejected through the localized sticker error envelope.
- Collect success, idempotent hit, invalid path, quota, query, and store outcomes are observable through low-cardinality metrics.
