---
type: Learning
title: "A guard that names its subject file, or that supplies its own inputs, is not a floor"
description: "Source guards and enumeration guards silently stop covering a codebase as it grows: one reads a file by name and cannot see the file added beside it, the other only covers the paths its own fixtures exercise. Derive the set where it is derivable; where it is not, guard the enumeration itself."
tags: ["testing", "guards", "reconcile", "observability", "maintenance"]
timestamp: 2026-09-08T02:30:00Z
source: self
status: pending
---

# A guard that names its subject file, or that supplies its own inputs, is not a floor

## The rule

A guard exists to fail when a property stops holding. Two very common shapes stop
being able to fail as the codebase grows, and both keep passing while they do it.

**1. A source guard that names its subject file covers that file, not the
property.** `os.ReadFile("reconcile.go")` is a guard about `reconcile.go`. The
day someone adds `reconcile_p1.go` next to it, the property is unguarded there
and nothing says so.

**2. An enumeration guard covers the paths its own fixtures exercise.** A test
that drives create/update/delete and asserts each emits an audit entry proves
nothing about a fourth path it never calls — and adding that path does not make
the test red.

Prefer **discovery** over enumeration wherever the set is derivable from the
source: read the directory, read the constants, read the function names. Where
the set genuinely cannot be derived, the enumeration needs a guard of its own
that derives it and compares.

And when a change adds a path of a kind some guard enumerates, **registering it
is part of the change**, not follow-up work.

## Why (the evidence)

In `Mininglamp-OSS/octo-server`, one reconcile cost rule — *evaluate predicates as
a per-row SELECT flag over the LIMIT-bounded base rows, never as a WHERE filter,
so LIMIT bounds rows EXAMINED and not rows RETURNED* — was violated three times
across three consecutive changes, and the guard could not see any of them:

* P0 wrote `TestReconcileQueriesAreBounded` and a cost guard, both reading
  `reconcile.go`.
* P1 added `reconcile_p1.go`. Neither guard covered it, and `queryI2Page` shipped
  with `g.status <> 2` in its WHERE. Found by review, fixed with a second guard
  file rather than by widening the first.
* P2 added `reconcile_p2.go`. Neither of the two guard files covered it, and
  `queryMissingAllMemberGroupPage` shipped with `g.id IS NULL` in its WHERE.
  Found by review, fixed with a third guard file.

The failure mode is the nasty one: `g.id IS NULL` matches **nothing** in a healthy
database, so the statement returns no rows, reports no violations, looks perfectly
healthy — and walks the whole table on every tick, most expensively when there is
nothing wrong.

Contrast, same repository, same three changes: `TestGroupNoLegacyResponseError`
discovers its file list with `os.ReadDir(".")`. It needed no maintenance across
any of them, and it covered every new file the day it landed. Its comment records
why it was converted from a hard-coded list: *"a hard-coded list cannot see a new
file — which is the failure mode that matters, because nobody adds a handler by
editing api.go, they add it by writing a new one."*

The enumeration half showed up in the same change. `TestEveryWritePathEmitsAnAuditEntry`
was green for the entire time a new create-time membership write emitted no audit
entry at all, because the test never passed `agent_uids` to the create it drives.
And `TestWritePathsRevalidateTheActorSpaceSeatInTx` declared its scope as "the
callers of `requireSpaceSeatsTx`" — a scope that excluded, by construction, a new
write path that takes the same lock directly instead of through that helper. Both
were found by re-reading the spec against the guards, not by any test failing.

## How to apply it

- **Deriving the set**: `os.ReadDir(".")` for files; a `func (p *Project) scan…()`
  prefix scan for scan entry points; a `query…Page(` signature scan for paged
  queries; reading exported constants out of the source for label enumerations.
- **When you must enumerate**, add the meta-guard:
  `TestEveryAdmissionEntryConstantIsInTheGuardLists` reads the constants from the
  source and compares them to the hand-written lists, so a new constant cannot be
  forgotten.
- **Guard against vacuity** in both shapes. `require.NotZero(t, selects, "no
  SelectBySql found; this guard would pass vacuously")` costs one line and turns
  "the pattern stopped matching" from a silent pass into a failure.
- **Declare exemptions with their argument next to them**, in a map keyed by the
  exempt predicate, rather than loosening the shared rule. A rule with a named
  exception still fails on everything else; a loosened rule fails on nothing.
- **Prove the guard can fail.** Mutate the source, watch the assertion fire, revert.
  Both P2 guards were confirmed this way; without it a new guard file is only a
  claim of coverage.
