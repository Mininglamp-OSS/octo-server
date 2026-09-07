---
type: Learning
title: A comment asserting a guarantee needs a test
description: "When a comment claims a mechanism ('the reconcile job is the backstop', 'the next tick continues'), the claim must be pinned by a test whose failure mode is that exact claim — three separate bugs in one PR were comments describing code that did not exist."
tags: ["review", "comments", "testing", "invariants"]
timestamp: 2026-09-06T03:40:00Z
status: pending
source: project-p0-foundation
---

# A comment asserting a guarantee needs a test

## What happened

One PR, four review rounds, three bugs of the same shape:

1. The Space-removal cascade processed one 200-row page and returned nil. The comment
   said the reconcile job was "the backstop that makes a stalled walk visible" — but the
   reconcile job is read-only by design and never fixes anything. Rows past the page were
   leaked permanently.
2. The reconcile page cap's comment said "the next tick continues from a fresh cursor".
   Every cursor was a function-local, so every tick restarted from the top; past
   `maxPages × limit` rows, nothing was ever examined again.
3. Batch endpoints printed a `not_attempted` label. On the remove path the handler drove
   execution, so the label was true; on the add path the service had already run every
   target, so the same label was false — and hid committed work plus its audit entries.

## The pattern

A comment that describes a *guarantee* — something a later pass will do, something that
holds across calls, something another component is responsible for — is a specification.
An untested specification rots silently, and it rots into exactly the form readers trust
most: prose next to the code, written by someone who understood it at the time.

## The rule

If a comment asserts that X happens later / elsewhere / automatically, then either:

- there is a test whose failure message quotes that claim, so the rot is caught by CI; or
- the comment is rewritten as a description of current behaviour, with the deferred work
  filed as an issue/brief entry.

"Describing current behaviour" is fine and needs no test. "The worker will pick this up",
"the caller must", "the next tick continues" are guarantees, and they are the ones that rot.
