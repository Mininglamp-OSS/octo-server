---
type: Learning
title: A reproduction is not a root cause
description: When a report proves a bug by toggling one input, fixing that input can turn the reproduction green while the vulnerability stays fully open.
tags: ["security", "review", "root-cause", "pentest"]
timestamp: 2026-08-07T16:19:08Z
status: pending
source: reminder-sync-membership-scope
---

# A reproduction is not a root cause

## What happened

A pentest report demonstrated a data leak by deleting the `X-Space-Id` header:
with it, `403`; without it, a 26 KB dump of other tenants' data. It filed the
finding as an `X-Space-Id` authorization bypass and recommended enforcing the
header.

The header was real, the `403` was real, the dump was real — and the diagnosis
was wrong. The handler never read the validated `space_id`. Removing the header
walked past a middleware; the leak lived one layer down, in a SQL branch with no
membership predicate at all. A caller who *kept* a header for a space they
legitimately belonged to got the identical dump.

Implementing the recommendation would have turned the report's exact
reproduction green and closed the ticket with the vulnerability untouched.

## The generalisable shape

A reproduction proves *reachability*. It does not identify *which* missing check
allowed it. Those coincide often enough that treating them as the same thing
usually works — which is exactly what makes it dangerous when they diverge.

The tell: **the reproduction toggles one input, and the report names that input
as the cause.** The toggle proves there is a path; it does not prove the toggle
is the gate. Ask what else reaches the same code with the toggle left alone.

## What to do

- Trace from the **data** back to every path that returns it, not forward from
  the reproduction. Here: what query returns these rows, and what constrains it?
  The answer — nothing — was visible without reproducing anything.
- Before accepting an attribution, construct the **variant that keeps the
  toggle**. If it still leaks, the toggle was not the gate.
- Write the primary regression test **against the variant, not the
  reproduction**. A test derived from the report's steps passes for a fix that
  only satisfies the report. The test here keeps a valid `X-Space-Id` header and
  still demands non-member rows be absent, so a middleware-only fix cannot pass
  it.
- When correcting a report's severity or attribution, keep its reproduction in
  the record too. It is real evidence, and reviewers who saw the report need the
  mapping between what they read and what was fixed.

## Cost of getting it wrong

Highest of any class of security bug: everyone believes the issue is closed. The
ticket is resolved, the retest passes, the finding leaves the tracker — and the
data keeps leaking, now with documentation asserting it does not.
