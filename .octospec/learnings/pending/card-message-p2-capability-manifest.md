---
type: Learning
title: "Capability/manifest endpoints must source every field from a single in-package authority, guarded by a drift test"
description: A server endpoint that advertises what it supports (capability manifest, feature flags, accepted-format set) must derive each value from the same constant/authority the enforcement path reads — never a re-typed literal in the handler — and ship a test asserting the advertised set equals the enforced set, so the wire contract can't claim a capability the server doesn't honor.
tags: ["wire-contract", "bot-api", "capability-discovery", "testing", "review"]
timestamp: 2026-07-08T15:00:00Z
# --- octospec extension fields ---
source: self
origin_task: card-message-p2-capability-manifest
origin_pr: self
status: pending
candidate_rule: error-handling
---

# Capability/manifest endpoints must source every field from a single in-package authority, guarded by a drift test

## Context

PR-D (D12) added `GET /v1/bot/card/profile`, a manifest telling card producers
which profiles/limits the deployment accepts. The tempting shortcut is to
hardcode the answer in the handler:

```go
"profiles": []string{"octo/v1", "octo/v2"},          // literal — drifts silently
"max_nodes": 200,                                     // literal — drifts silently
```

The failure mode is subtle and dangerous: the **validator** (`cardmsg.Validate`
/ `interactiveByProfile`) is the real authority for what's accepted, but the
**manifest** is a *second copy* of that knowledge. When someone later adds
`octo/v3` to the validator, or tightens `MaxNodes`, the manifest keeps
advertising the old set — producers feature-detect against a lie, and the
mismatch only surfaces as confusing `400`s in production.

## What to do instead

1. **Single authority.** Put the accepted set / limits in one place and have
   *both* the enforcement path and the manifest read it. PR-D introduced
   `cardmsg.AcceptedProfiles()` backed by an `acceptedProfiles` slice, and
   refactored `interactiveByProfile` (the validator) to derive its accepted set
   from the *same* slice. Scalar limits are referenced as the exported constants
   directly, never re-typed.
2. **Drift-guard test.** Assert the advertised set equals the enforced set:
   every profile the manifest returns must be accepted by the validator, and
   unknown profiles must be rejected (`TestAcceptedProfilesSingleAuthority`).
   This catches a future refactor that reintroduces a hardcoded copy.
3. **Additive-only contract test.** Pin the exact field set so a rename/removal
   fails CI (`TestBotCardProfile_AdditiveContractFieldSet`); new capabilities
   are added as new fields.
4. **Feature-detect endpoints don't gate on the feature flag.** A disabled
   deployment must still return the full manifest with `enabled:false` — the
   whole point is that a producer can read it and fall back. Only the
   enforcement path (send/edit) rejects. Test both halves together so they
   can't drift.

## Why it's a rule candidate

Any "server tells clients what it supports" surface has this hazard —
capability manifests, accepted-content-type lists, feature-flag endpoints,
version negotiation. The two-copies-of-one-truth bug is easy to introduce and
hard to notice in review. A rule ("manifest/capability values come from the
enforcement authority + a drift test") would make it a checklist item rather
than a case-by-case catch. Filed against `error-handling` / wire-contract as the
nearest existing rule home; a reviewer decides whether it warrants its own rule.
