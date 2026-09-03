---
type: Learning
title: "Learning: a test double has to model the behaviour that breaks you, not the behaviour that passes"
description: A double written to make an assertion pass can certify a fix that production cannot perform; model the collaborator's destructive and miss-path behaviour first, then confirm the test fails without the fix.
tags: ["testing", "test-doubles", "verification", "security", "session"]
timestamp: 2026-09-02T00:00:00+08:00
# --- octospec extension fields ---
task: oidc-oauth2-provider-abstraction
status: pending
---
# A test double has to model the behaviour that breaks you

## What happened

A logout was supposed to disconnect only the device that initiated it. The
device identifier lives in the cached session payload, so the handler read it
through the session store. A test asserted precisely the right thing: one
`KickDevice` call with the right flag, and no `Kick`-everything call.

The test passed. Production kicked every device on every logout.

The handler called `InvalidateCurrentToken` first, and that call **deletes the
session record** — the same record the device lookup then reads. In production
the read always missed and fell through to the conservative "kick everything"
branch. The feature had never worked.

The double is why nobody noticed:

```go
func (s *stubTokenStore) InvalidateCurrentToken(_ context.Context, _, token string) error {
    s.invalidated = append(s.invalidated, token)   // records the call
    return nil                                     // ...and deletes nothing
}
```

It also returned an `error` on a cache miss, where the real store returns a
record with `TTL: -2` and a **nil** error — so even the fallback path was being
exercised through a branch production never takes.

Both gaps came from writing the double to satisfy the assertion rather than to
imitate the collaborator.

## Why the usual instinct fails

A double is normally judged by "does it provide what the code under test needs?"
That question is satisfiable by a double that only ever behaves well, and a
mutating collaborator's *destruction* is exactly what the code under test needs
to survive. The interface gives no hint: `InvalidateCurrentToken` returning
`error` says nothing about it deleting the row a sibling method reads.

Ordering bugs are the specific casualty. Nothing about "read the flag" and
"invalidate the token" reads as order-dependent until you know that one erases
the other's input — and a double that erases nothing makes both orders pass.

## Rule of thumb

- Before writing a double, ask what the real collaborator **destroys, evicts, or
  invalidates**, and what it returns on the *absent* path. Model those two
  first; the happy path is the easy part.
- Copy the real miss semantics exactly. "Returns an error when not found" versus
  "returns a zero value with a nil error" selects different branches, and the
  branch you didn't model is the one that runs in production.
- **Confirm the test fails without the fix.** For a bug already understood, this
  is one command: revert the fix, run, see red, restore. A test written after
  the fix has proved nothing until it has failed once. This one finding is worth
  the whole practice — the test had been reported as passing evidence.
- When a double stands in for something with a lifecycle, name the modelled
  behaviour in a comment on the double itself. The next person to extend it
  needs to know that deleting-on-invalidate is load-bearing, not incidental.
