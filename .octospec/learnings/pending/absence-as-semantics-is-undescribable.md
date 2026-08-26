---
type: Learning
title: "Absence carrying meaning cannot be described — codegen clients get it wrong by default"
description: "When a missing key means something other than the zero value, no schema language can express it, so generated clients silently implement the opposite of the intent. Carry the state in a field instead."
tags: ["api", "wire-contract", "openapi", "review"]
timestamp: 2026-08-25T00:00:00+08:00
# --- octospec extension fields ---
source: self
origin_task: featuregate-user-scoped-flags
origin_pr: Mininglamp-OSS/octo-server (featuregate user-scoped flags)
status: pending
candidate_rule: error-handling
---
# Absence carrying meaning cannot be described

A response that must express three states with only two values is tempting to
solve by dropping the key:

```json
{"flags": {"docs_on": true, "drive_on": false}}   // reader must infer a third state
```

Here `true`/`false` are decisions and **a missing key means "could not be
computed — keep whatever you had"**. That design is sound at the semantic level:
it is what stops one Redis blip from turning into "every user loses the feature",
because a bare `false` is indistinguishable from a real "off".

The problem is that the contract is **unexpressible**. OpenAPI (any version),
JSON Schema, and protobuf can all say a property is optional; none of them has a
vocabulary for "absence carries a meaning distinct from the zero value".
It survives only as prose in a `description`.

That matters because of who reads it. A generated client deserializes into
`map[string]bool` / `Map<String, Boolean>` and the distinction is gone before any
application code runs — the client will read a missing key as `false` via the
language's own zero-value rules. **The default behaviour of every codegen
consumer is the exact failure the design existed to prevent**, and it fails
silently, on the rare path, in production.

## What to do instead

Put the third state in a field, so it is data rather than an inference:

```json
{"flags": {"docs_on": true}, "unavailable": ["drive_on"]}
```

`unavailable` is an ordinary `array of string`: fully describable, impossible to
lose in deserialization, and it leaks nothing extra (it says "not computed", not
*why*).

## How to spot it in review

Ask of any response field: **"is there a state here that is encoded by something
other than a value?"** Absence, `null`, empty string, and empty array all qualify.
If yes, either the state moves into a value, or the contract only holds for
hand-written clients — and that assumption needs to be stated out loud, because
it stops holding the day someone points a generator at the spec.

Related: `json-nil-vs-empty-slice.md` — the request-side counterpart, where
collapsing nil and empty erases a caller's bug. Same root cause, opposite
direction: absence is a state, and treating it as a value (or a value as
absence) loses information.
