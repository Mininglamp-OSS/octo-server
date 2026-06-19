---
type: Rule
title: Rate limiting
description: Per-UID/per-route rate limiting requirements for service endpoints.
tags: ["rate-limit", "throttle"]
timestamp: 2026-06-19T00:00:00Z
# --- octospec extension fields (OKF-permitted; consumers must preserve) ---
id: rate-limit
tier: repo
priority: 80
load_bearing: true
inject_when:
  paths: ["modules/**/*.go", "base/common/service_*.go", "internal/**/*.go"]
  touches: ["rate-limit", "throttle"]
source: self
supersedes: []
---

# Rate limiting

Use the shared middleware for request-frequency limiting. Do not hand-roll a
Redis counter for generic HTTP throttling.

## Rules

- Authenticated routes: mount `SharedUIDRateLimiter`.
- Unauthenticated routes: mount `StrictIPRateLimitMiddleware`.
- Never hand-roll a Redis counter for generic request-frequency limiting.

## Exception (intentional)

Per-resource cooldowns keyed by a business identity (phone / email /
bind-session) that the IP/UID buckets cannot express may use a hand-written
Redis counter — e.g. `sms_rate_limit:{zone}@{phone}`, `email_rate_limit:{email}`,
OIDC bind attempt caps. These are intentional and not a violation.

## Testing note

Tests hitting UID-limited routes must reset the bucket in setup
(`ratelimit:uid:*`); it persists in Redis and is NOT cleared by
`CleanAllTables` (see `category` test's `resetUIDRateLimit`).
