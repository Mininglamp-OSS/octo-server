---
type: Task
title: "Task: sticker-edit-shortcode-observability"
description: Add custom-sticker edit/sort, shortcode/keywords autocomplete metadata, and production observability/extra upload throttling.
tags: ["sticker", "wire-contract", "database", "rate-limit", "observability"]
timestamp: 2026-06-30T00:00:00Z
slug: sticker-edit-shortcode-observability
source: user
---

# Task: sticker-edit-shortcode-observability

## Goal

Complete the next custom-sticker increment after upload-handle hardening:

- Users can edit their own custom sticker metadata and control list order.
- Custom stickers can carry searchable `shortcode` and `keywords` metadata for client-side autocomplete.
- Sticker upload/registration emits low-cardinality metrics and structured logs, and sticker uploads receive an explicit endpoint-level throttle.

## Load-bearing list

- **`PUT /v1/sticker/user/:sticker_id` wire contract**: partial update for `placeholder`, `sort`, `shortcode`, and `keywords`; omitted fields keep existing values. Cross-user or missing stickers return the existing not-found envelope.
- **List ordering**: live stickers sort by `sort ASC, id DESC`, preserving legacy newest-first behavior for old rows where `sort=0`.
- **Shortcode uniqueness**: non-empty shortcode is unique only among live stickers owned by the same uid. Soft-deleted rows release the shortcode. Check runs inside the same user-row-lock transaction used by sticker quota, avoiding same-user concurrent duplicates without a plain unique index.
- **Keywords storage**: request/response exposes `keywords` as a JSON array; DB stores a bounded JSON string in `VARCHAR(255)`.
- **Error envelope**: new sticker validation errors use registered `pkg/errcode` codes and zh-CN translations; legacy raw error responses remain forbidden in `modules/sticker`.
- **Observability**: metrics use bounded labels only (`result`), never uid/path/shortcode/keyword labels. Logs may include uid, format, size, dimensions, reject reason, and handle-required posture.
- **Rate limit**: sticker upload gets an additional explicit limit using shared wkhttp middleware, not a hand-written Redis counter.

## Out of scope

- Full `modules/file` i18n migration. The file module still has legacy response sites; this task only adds sticker-specific observability and endpoint throttling.
- Server-side sticker search endpoint. `shortcode` and `keywords` are returned for client-side autocomplete in this phase.
- Unique DB index for shortcode. Soft delete release semantics and per-uid scoping are enforced transactionally in code.

## Acceptance

- `PUT /v1/sticker/user/:sticker_id` edits own sticker metadata, supports `sort=0`, and rejects cross-user updates as not found.
- `GET /v1/sticker/user` returns `sort`, `shortcode`, and `keywords`, ordered by `sort ASC, id DESC`.
- `POST` and `PUT` validate shortcode/keywords, reject same-user live shortcode conflicts, allow different users to reuse shortcode, and allow reuse after delete.
- Sticker upload emits counters for success, size/format/dimension/path rejection, handle issued/disabled, and upload failure.
- Sticker registration emits counters for success, missing/invalid handle, quota exceeded, shortcode conflict, validation, query/store failures.
- Sticker-related Go files are gofmt-formatted; focused sticker/file tests pass; i18n extract/lint checks pass after adding error codes.
