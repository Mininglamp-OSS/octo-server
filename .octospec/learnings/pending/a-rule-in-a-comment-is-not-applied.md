---
type: Learning
title: "Learning: a design rule stated in a comment is not applied by being stated"
description: A diff can articulate a correctness rule in a comment and violate it three lines away, because the surrounding code predates the articulation and nothing re-checks it. Assert the rule mechanically, not narratively.
tags: ["review", "concurrency", "lost-update", "sql", "testing", "verification"]
timestamp: 2026-09-03T00:00:00+08:00
# --- octospec extension fields ---
task: bot-agent-hosting
status: pending
---
# A design rule stated in a comment is not applied by being stated

## What happened

A change added a fourth column to an existing multi-column `UPDATE`, and reasoned
carefully about why the new column must be *absent from the statement* when the
caller did not report it:

```go
// The pointer is carried all the way down to the SetMap rather than being
// resolved into "the value we just read": an absent field must leave the column
// untouched in SQL. Reading the stored value and writing it back would look
// equivalent but loses concurrent updates — "A reads old → B writes new →
// A writes old back".
var hosting *string
```

Directly above it, in the same function:

```go
platform, version, plugin := req.AgentPlatform, req.AgentVersion, req.PluginVersion
if platform == "" {
    platform = robot.AgentPlatform      // ← the value read at the top of the request
}
```

The three sibling columns got exactly the treatment the comment rejects. Worse,
the same diff removed a "skip the UPDATE when nothing changed" shortcut, so the
write went from conditional to unconditional — **widening** the race rather than
leaving it where it was. The new request shape the feature introduced ("report
only the new field") wrote all three old columns from a stale read.

Two reviewers found it independently. The test suite was green throughout.

## Why the comment didn't help

The rule was true, well-argued, and correctly applied *to the field being added*.
The sibling code predated the articulation: it was already there, it already
worked for a single writer, and nothing in the change re-examined it in the light
of the sentence just written above it. A comment records a decision; it does not
audit the code around it.

There is a second reason the code looked fine: **read-then-write-back and sparse
write are observationally identical to every test with one writer.** The value
comes back unchanged either way. So both the reasoning artifact (the comment) and
the verification artifact (the tests) were blind to the same thing.

## The rule

**When you write a correctness rule into a comment, grep the same function for
other code the rule applies to — and then assert the rule mechanically.**

For SQL specifically, "which columns are in the statement" is directly assertable
and worth asserting, because no DB-observable test can distinguish the two
implementations:

```go
matcher := sqlmock.QueryMatcherFunc(func(_, actual string) error {
    captured = actual
    return nil
})
// ...
assert.NotContains(t, captured, "col_the_caller_did_not_report")
```

The end-to-end complement is an **out-of-band write**: change the column from a
third party between the request's read and its write, then assert the out-of-band
value survived. That fails against write-back and passes against sparse write —
which "the value is unchanged" never does.

## Generalisation

- **A merge helper is a lost update wearing a convenience.** `if x == "" { x =
  stored.X }` reads as defaulting and behaves as a blind write. Under a single
  writer it is invisible; under two it silently reverts. If concurrent writers are
  possible — and "nothing prevents it today" means they are — the sparse form is
  not an optimisation, it is the correct one.
- **Watch for a diff that turns a conditional write into an unconditional one.**
  That change alone can convert a latent race into a reachable one, even when
  every individual value written is correct.
- **When a test comment claims it distinguishes two implementations, check that it
  does.** The same task shipped a test asserting "value preserved AND timestamp
  advanced" and claimed that pinned sparse-write. It does not: the rejected
  implementation writes the same value plus the timestamp, so all assertions pass.
  A claim about what a test rules out is itself a claim that needs checking.
