---
type: Learning
title: "Learning: a feature needs one test that goes through real assembly"
description: Tests that inject a dependency through a helper cannot show that production wiring exists; a missing registration call passes every one of them while the feature is inert.
tags: ["testing", "wiring", "dependency-injection", "provider", "hook"]
timestamp: 2026-08-26T00:00:00Z
# --- octospec extension fields ---
task: file-extension-policy-dynamic-config
status: pending
---
# A feature needs one test that goes through real assembly

## What happened

`modules/file` gained a package-level policy snapshot fed by `SetPolicySettings()`.
`File.New()` built the `SystemSettings` but never called it. Every test injected a
fake through a `useSettings()` helper, so the whole suite was green while the
feature was **inert in production**: manager writes landed in the DB, returned 200
with `applied: true`, and reached no upload gate and no appconfig payload. Nothing
logged an error, because nothing was wrong — the snapshot simply kept taking its
"not mounted" branch.

Review caught it, not the tests.

## Why the tests could not catch it

They all started *after* the wiring:

```go
useSettings(t, fakePolicySettings{...})   // == SetPolicySettings(fake)
assert.True(t, IsBlockedExtension(".pdf"))
```

That asserts the policy layer works given a mounted provider. It says nothing
about whether anything mounts one. The number of such tests is irrelevant — a
hundred of them prove the same thing.

## The rule

**Any registration-style wiring — provider, hook, probe, `init()` side effect,
middleware mount — needs at least one test that reaches it through the real
construction path**, not through the injection helper. In this repo that is
`testutil.NewTestServer()` (`module.Setup → New(ctx)`), driving the actual
entry point and asserting an observable effect.

Cheap zero-infra fallback when a full server is too heavy: a source guard
asserting the constructor body contains the registration call. Weaker (it
matches text, not behaviour) but it catches deletion, which is the common case.

## Cost

`modules/file` had no infra-dependent tests before this; adding one made the
package require MySQL/Redis/WuKongIM. That is worth it here — the alternative was
shipping a dead feature that reports success. Keep the integration test to the
minimum that exercises assembly and leave the rest as unit tests.

## Where this repo is exposed

The same registration pattern is used by the appconfig limits provider, the
built-in-blocked probe, `cardmsg` gates, and the module `init()` registrations —
each is one deleted line away from the same silent failure.
