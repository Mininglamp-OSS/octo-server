---
type: Task
title: "Task: filespace-documents-backend"
description: Scope the octo-server Filespace document backend changes onto latest main with Octospec load-bearing rules.
tags: [filespace, document, space-isolation, error-handling, rate-limit, testing]
timestamp: 2026-07-08T17:05:52+08:00
slug: filespace-documents-backend
upstream: Mininglamp-OSS/octo-server#552
source: self
---

# Task: filespace-documents-backend

## Goal

Prepare the `octo-server` Filespace document backend PR so it contains only the business changes required for Filespace document/asset functionality, is based on the latest `Mininglamp-OSS/octo-server` main branch, and satisfies Octospec verification before review.

## Background

The PR branch `filespace-v1` was rebased on the latest `origin/main`, then narrowed to the Filespace backend surface. The intended scope is the new `modules/document` implementation plus the required file/search/module/i18n integration. Unrelated base app configuration, group avatar, message/sidebar, user sticker upload, voice adapter, seed script, and static/config changes are out of scope.

Earlier broad-merge verification exposed compile blockers in unrelated or loosely related areas, which must stay resolved while narrowing the PR:

- `modules/base/app` redeclares `StatusDisable` and `StatusEnable`, and mixes typed `Status` with `int`.
- `modules/file/api.go` has unused imports and calls `f.stickerUploadHandlers(r)` even though the method is not present in the branch.

Octospec review also identified load-bearing concerns in the Filespace document API:

- Document read/list/state paths must not leak tenant assets, spaces, bindings, members, events, or storage paths across space ownership or membership boundaries.
- User-facing HTTP errors must use localized error handling with registered `pkg/errcode` codes.
- Authenticated document routes must keep the shared UID rate limiter and space middleware.

## Load-bearing list

- `space-isolation`: every document, asset, space, binding, member, event, preview, and download access path must enforce tenant space and user ownership or membership. Do not expose resources solely because they share `tenant_space_id`.
- `error-handling`: user-facing document/file errors must use localized error envelopes through `httperr.ResponseErrorL` and registered `pkg/errcode` entries. Do not add raw `c.ResponseError`, `c.JSON`, or `AbortWithStatusJSON` responses for user-facing failures.
- `rate-limit`: authenticated `/v1/documents` routes must remain behind `SharedUIDRateLimiter`, `AuthMiddleware`, and `SpaceMiddleware`.
- `testing`: add or keep focused tests for access isolation, preview/download authorization, business route behavior, and regression coverage for error envelopes.
- `commit-style`: PR description and later commits must stay in English conventional style and link the task/PR context.

## Out of scope

- `octo-web`, `octo-deployment`, and `octo-matter` changes.
- Sticker upload implementation unrelated to Filespace document assets.
- Voice adapter, group avatar, message/sidebar, and base app legacy refactors unless they are strictly required by Filespace document behavior.
- Demo seed scripts and static config changes unless explicitly required for a reviewed demo scenario.
- Broad formatting, generated churn, or unrelated dependency/config updates.

## Acceptance

- PR diff is narrowed to Filespace document backend business scope and any necessary integration points.
- Branch remains based on the latest `Mininglamp-OSS/octo-server` main branch.
- `go test ./...` compiles, with unrelated local infrastructure failures documented if they cannot be reproduced cleanly.
- Focused tests pass for `modules/document`, affected `modules/file`, affected `modules/search`, and affected `modules/common` paths.
- Document list/state/read/preview/download paths only return data the authenticated user can access.
- No document response exposes internal storage paths unless explicitly authorized and intended by the API contract.
- User-facing errors in touched document/file paths use localized registered error codes.
- CI for the PR is green or any remaining failure is clearly outside this task and documented.
