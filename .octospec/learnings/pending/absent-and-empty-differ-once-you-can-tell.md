---
type: Learning
title: "Learning: introducing a pointer to fix concurrency also introduces a distinction the old contract depended on not existing"
description: Switching a field from string to *string to keep absent values out of a SQL statement makes "omitted" and "explicit empty" distinguishable — and if the previous contract treated both as no-ops, the explicit-empty case silently becomes destructive.
tags: ["api-contract", "concurrency", "sql", "data-loss", "backward-compatibility"]
timestamp: 2026-09-03T00:00:00+08:00
# --- octospec extension fields ---
task: bot-agent-hosting
status: pending
---
# Introducing a pointer to fix concurrency introduces a distinction the old contract depended on not existing

## What happened

A handler merged optional telemetry fields against the stored row:

```go
if merged.plugin == "" { merged.plugin = robot.PluginVersion }   // "empty means unchanged"
```

Review correctly identified this as a lost update: it writes back the value read
at the top of the request, so two concurrent writers interleave as
`A reads old → B writes new → A writes old back`. The fix was to make the fields
`*string` and only put them in the `SetMap` when non-nil, so an unsupplied field
is absent from the SQL rather than written back.

That fix was right, and it shipped with a comment asserting the wire contract was
unchanged, "because omitting a field and sending it as `\"\"` were already
indistinguishable".

They *were* indistinguishable — under the old code, where both were no-ops. The
pointer made them distinguishable, and only one branch was updated. A non-nil
pointer to `""` now reached the statement verbatim:

```
seed: plugin_version=9.9.9
then: {"plugin_version":""}
before -> "9.9.9"   preserved
after  -> ""        wiped
```

The trigger is mundane: any client marshalling a struct without `omitempty` sends
`""` for fields it does not populate. This endpoint was the reconnect path, so it
repeated for the life of the deployment, at HTTP 200, with nothing in the log, and
afterwards the row was indistinguishable from one that had never reported.

## The rule

**When you introduce a distinction the old code could not make, enumerate what the
old contract said about each side of it — separately.**

The reasoning that failed here compressed two cases into one sentence:

> omitted and `""` were already indistinguishable ⟹ the contract is unchanged

The first clause is a statement about the *old* implementation. The conclusion is a
claim about the *new* one. Between them sits the whole change. Written out per
case, the gap is obvious:

| | old behaviour | new behaviour |
|---|---|---|
| field omitted | no-op | absent from SQL ✓ same |
| field `""` | no-op | **written** ✗ destructive |

Sparse writes and "empty means unchanged" are compatible — the lost update came
from *substituting the value just read*, not from *skipping the column*. So the
correct predicate is "supplied AND non-empty" for fields whose old contract
treated empty as unchanged:

```go
if report.Legacy != nil && *report.Legacy != "" {
    set["legacy_col"] = *report.Legacy
}
```

If a *new* field genuinely needs explicit-empty to mean "clear", that asymmetry is
fine — but it is a contract difference, and it should be stated as one rather than
described as "unchanged".

## Generalisation

- **Nullability is a wire contract change even when the JSON shape is identical.**
  `string` → `*string` alters nothing a client serializes and everything the server
  can perceive. Same for adding `omitempty`, or moving from a value to a
  presence-tracking wrapper.
- **A comment claiming backward compatibility is a claim to verify, not a note.**
  This one was carried into the commit message and the PR description, so three
  artifacts asserted the safety of the same unverified thing. The cheapest check is
  a test that seeds a value, sends the empty case, and asserts the value survived —
  which is precisely the test the suite did not have.
- **The freshly written rule does not audit the code it sits beside.** The same
  task hit this twice: once when a lost-update rule in a comment sat above three
  columns violating it, once when a superseded rule stayed in the spec next to the
  rule replacing it — and the stale one described the behaviour that would have
  prevented this bug. Superseding means deleting.
