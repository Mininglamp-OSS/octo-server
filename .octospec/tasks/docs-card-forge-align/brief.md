---
type: Task
title: "Task: docs-card-forge-align"
description: Align the built-in docs.access-request@0.3.0 card layout with the latest Forge card while preserving the server-owned interaction and metadata contracts.
tags: ["card", "wire-contract", "trust-boundary", "testing"]
timestamp: 2026-07-23T00:00:00+08:00
slug: docs-card-forge-align
upstream: self
source: self
---

# Task: docs-card-forge-align

## Goal

Render `docs.access-request@0.3.0` with the latest Forge visual structure:
the compact header, full-bleed accent content surface, semantic badges, and
full-bleed footer. Keep the server authoritative for metadata, deep links,
localized copy, validation, and action dispatch data.

## Pre-release correction

The platform-card documentation normally freezes a published L1 version. The
owner has explicitly classified `docs.access-request@0.3.0` as still being in
cross-repository alignment and requested an in-place correction instead of a
new `0.4.0`. This exception is valid only if the current 0.3.0 output has not
been rolled out to production. Confirm that condition before merge.

## Load-bearing behavior

- Existing `docs.access-request@0.2.0` output remains unchanged.
- The 0.3.0 pending view keeps the production action IDs
  `docs-access-approve` and `docs-access-deny`.
- Submit `data` remains server-authored and retains the existing
  `owner/action_type/decision/doc_id/request_id` contract plus the context
  required by the result finalizer.
- The declared deny input remains `deny_reason`, hidden, and capped at 300
  runes; clients cannot invent a different input key.
- `Registry.Render` remains the only metadata/envelope assembly boundary.
- Cards opting into the aligned layout emit stable runtime compatibility
  profile `render_profile: "octo-chat/v1"`; the manifest pins the exact Forge
  artifact `octo-chat@1.2.0-rc.1`.
- All caller-controlled text continues through the existing bounding and
  escaping helpers; all URLs remain absolute HTTPS URLs produced or validated
  by the server.

## Out of scope

- No changes to callback routing, authorization, idempotency, or card action
  finalization semantics.
- No changes to `docs.access-request@0.2.0`.
- No dark theme or additional Render Profile generation.
- No changes to other card templates.

## Acceptance

- Pending, approved, and rejected 0.3.0 cards match the Forge layout structure
  while retaining the backend interaction contract.
- The rendered type-17 envelope includes `render_profile: "octo-chat/v1"`.
- The manifest records `octo-chat@1.2.0-rc.1`.
- Existing registry, conformance, updater, and notify tests remain green.
- Focused golden/structural tests lock full-bleed header/content/footer,
  separators, semantic IDs, action IDs/data, and deny input behavior.
