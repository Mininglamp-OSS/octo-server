---
type: Rule
title: Error handling & i18n
description: User-facing errors must use the localized error envelope.
tags: ["error-response", "i18n", "wire-contract"]
timestamp: 2026-06-19T00:00:00Z
# --- octospec extension fields (OKF-permitted; consumers must preserve) ---
id: error-handling
tier: repo
priority: 85
load_bearing: true
inject_when:
  paths: ["modules/**/*.go", "base/**/*.go", "pkg/errcode/**", "pkg/**/httperr/**"]
  touches: ["error-response", "i18n", "wire-contract"]
source: self
supersedes: []
---

# Error handling & i18n

User-facing errors must go through the localized error envelope. Never write a
raw error response from a handler.

## Rules

- Use `httperr.ResponseErrorL` together with a registered `pkg/errcode` code.
- Never use raw `c.ResponseError` / `c.JSON` / `AbortWithStatusJSON` for
  user-facing errors.
- After touching error codes, run `make i18n-extract-check` + `make i18n-lint`.

## Why load-bearing

The wire contract for error responses is consumed by web and mobile clients; a
raw or unlocalized error breaks the client contract and i18n coverage.
