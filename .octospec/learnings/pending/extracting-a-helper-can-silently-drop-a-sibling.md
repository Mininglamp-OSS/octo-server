---
type: Learning
title: "Learning: extracting one concern out of a constructor can silently delete a neighbouring one"
description: When lifting a block out of an initialiser, the deleted region can contain unrelated wiring; nothing fails, because every test injects that dependency by hand. Grep the moved region's constructors for surviving callers.
tags: ["refactoring", "wiring", "testing", "security", "logout"]
timestamp: 2026-09-02T00:00:00+08:00
# --- octospec extension fields ---
task: oidc-oauth2-provider-abstraction
status: pending
---
# Extracting one concern can silently delete a neighbouring one

## What happened

A constructor did two unrelated things next to each other: it built the auth
provider, and — a few lines later, under its own `if` — it built the id_token
cache that RP-Initiated Logout depends on.

Lifting the provider construction into a shared factory meant replacing a region
of that function. The replacement dropped the id_token block along with it. The
result compiled, vetted clean, and passed the entire suite.

In production the field stayed nil, and the consequences were all silent:

- login stopped caching the id_token (the write was gated on the field);
- logout therefore never had an `id_token_hint`, so the logout-URL builder
  returned "no URL" on exactly that condition;
- the upstream IdP session was never ended. A user who logs out stays signed in
  at the IdP — on a shared browser the next person is one click from the account.

Losing single logout is a security control regression, and nothing anywhere said
so. The constructor function itself was still there; it simply had zero callers.

## Why the suite could not catch it

Every test for the affected feature assigned the dependency directly:

```go
o.idTokens = newFakeIDTokenStore()
```

Ten call sites across two files. Each is a reasonable unit test. Collectively they
guaranteed that **no test ever asked whether production wires the thing at all** —
the doubles modelled a wiring that no longer existed.

This is the mirror image of "a double must model the collaborator's behaviour": a
double can be perfectly faithful and still prove nothing about assembly. Both
failures were present in the same change, one file apart.

## Rule of thumb

- After lifting anything out of a constructor, **diff the removed region, not just
  the added one**. `git show <before>:<file>` and read what left.
- For every `newXxxStore` / `newXxxClient` in a package, check it still has a
  non-test caller. A constructor with zero production callers is the signature of
  this defect, and it is one grep:
  ```
  grep -rn "newRedisIDTokenStore" --include=*.go .   # → only the definition
  ```
  Worth a guard test where the package has several such constructors.
- Any feature whose tests all inject a dependency by hand needs **one** test that
  builds the object the production way and asserts the dependency is present.
  It does not need to exercise the feature — only to prove the wiring exists.
- Suspicion should rise, not fall, when a refactor is described as mechanical. A
  mechanical change to a *region* is only mechanical if the region contains one
  concern; constructors usually contain several.
