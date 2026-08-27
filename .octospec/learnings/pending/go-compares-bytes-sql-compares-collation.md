---
type: Learning
title: "Moving a comparison from SQL into Go silently swaps the comparator"
description: A refactor that relocates a predicate on an identifier from SQL into Go changes the comparison semantics, because the column has a collation and Go compares bytes. Both a lookup and an ordering built on that assumption were wrong on the same branch. If a change moves an identifier comparison across the Go/SQL boundary, the collation is part of what has to be shown unchanged.
tags: ["database", "correctness", "refactoring", "security"]
timestamp: 2026-08-25T00:00:00Z
# --- octospec extension fields ---
source: self
origin_task: space-removal-creator-handover-notice
origin_pr: Mininglamp-OSS/octo-server#804
status: pending
candidate_rule: patterns
---

# Moving a comparison from SQL into Go silently swaps the comparator

## Context

Batching a per-row member removal into one transaction moved two comparisons of
`space_member.uid` out of SQL and into Go. The column carries no explicit
`COLLATE` and inherits the database default, which CI pins to
`utf8mb4_general_ci`: case-insensitive, and — because folding lowercase into
uppercase maps `a`→`A` (0x61→0x41) — `_` (0x5F) sorts *after* the letters rather
than between the cases.

Both relocated comparisons were wrong, in different ways, and neither was visible
in review of the diff alone.

**1. A map lookup became case-sensitive.** The batch locked its targets with
`SELECT uid, role … WHERE uid IN ? … FOR UPDATE`, keyed a `map[string]int` by
`row.UID`, then looked entries up with the caller's spelling:

```go
roleByUID[r.UID] = r.Role      // the STORED spelling
...
role, active := roleByUID[uid] // the REQUESTED spelling
if !active { continue }        // silently skipped
```

The SELECT matches case-insensitively and returns rows under their stored
spelling. The map lookup is byte-exact. When the two differ the member is treated
as absent and skipped — no write, no downstream job, no cache invalidation — and
the handler answers 200. The pre-refactor path did both comparisons in SQL
(`SELECT … WHERE uid=?` then `UPDATE … WHERE uid=?`) and worked. On an endpoint
whose purpose is revoking access, "reports success, did nothing" is the worst
available failure shape, and it was introduced by a change whose commit message
asserted semantic equivalence.

**2. An ordering claimed to match the index did not.** Lock-ordering two writers
against each other requires both to sort by the same authority. One side ordered
by `FORCE INDEX (…)` — i.e. `general_ci`; the other used `sort.Strings` — i.e.
bytes. Measured on the real index:

```
index order (general_ci):  aab, abcdef01, app_9f2_bot, a_b
Go sort.Strings:           a_b, aab, abcdef01, app_9f2_bot
```

`a_b` moves from last to first, which is precisely the AB-BA the sort existed to
remove. The two orders coincide for `[0-9a-f]`-style identifiers, which is why
this survives casual testing — and why the guard, whose fixture used `m%04d`,
went red under the mutation aimed at it while being structurally unable to see
this.

## The correction that came one round later

The first version of this note recommended writing a Go comparator that mirrors
the column's collation, and offered `general_ci` folding as the way to do it.
**That advice was wrong**, and wrong in a way worth preserving rather than
editing out.

The column's collation is environment-dependent. This table is created without
an explicit COLLATE and inherits the database default: CI pins
`utf8mb4_general_ci`, MySQL 8.0 defaults to `utf8mb4_0900_ai_ci`, and production
runs the latter. The two order `_` oppositely (general_ci folds case so `_`
lands after the letters; 0900_ai_ci follows UCA and sorts punctuation before
them). So the mirror matched CI and inverted production, and CI could not catch
it.

The better question is not *which* collation to mirror. It is **why the lock
order is the index's at all**. A single `col IN (...) FOR UPDATE` takes its locks
during the index scan, so the order is the index's and the caller cannot specify
it — `ORDER BY` does not help, the locks are already held by the time the sort
runs. Expanding the batch into `UNION ALL` branches, one single-row equality
each, makes MySQL acquire in branch order, which is the caller's order. Collation
then leaves the question entirely, and the sort can be the most boring byte-order
comparison available.

Generalised: **when you find yourself reproducing a database's semantics in
application code, check whether you can stop depending on those semantics
instead.** Mirroring is a maintenance burden that is only ever as correct as your
model of the deployment; removing the dependency is checkable once.

## Rule

If a change relocates a comparison on an identifier across the Go/SQL boundary,
the collation is part of what must be shown unchanged. Specifically:

- **Prefer not relocating it.** Driving a loop from the rows SQL matched, rather
  than from the request slice, removes the second comparison instead of trying to
  make two comparators agree. That is the fix that does not need a premise.
- **Prefer removing the dependency over mirroring it.** If an ordering has to
  match the database's, look for a statement shape that lets the caller specify
  the order (`UNION ALL` of single-row lookups does; a range scan does not).
  A Go comparator that mirrors a collation is only as correct as your assumption
  about which collation the deployment uses — and if the table has no explicit
  COLLATE, that assumption is about a database default you do not control.
- **Test under every collation the code can meet, not the one CI happens to
  build.** A guard that creates its own tables with an explicit COLLATE catches
  what a guard trusting the ambient database cannot. If a test can only run under
  one collation, it is testing the environment as much as the code.
- **Vary the fixture's alphabet.** Identifiers restricted to digits and one case
  are the alphabet where byte order and collation order agree. A guard built on
  them cannot fail for this class no matter how hard you mutate the code. Include
  `_`, an uppercase letter, and a prefix pair.
  (Note: "same name, different case" cannot be used to introduce uppercase where
  a unique index is involved — under `general_ci` those are one key and the second
  insert is a 1062.)
- **Watch for the case-insensitive-match consequences beyond ordering**: an
  uppercase-variant identifier reaching an authorization or revocation path can be
  matched by SQL and missed by Go, which turns a security control into a no-op
  that reports success.

## Not this

Do not "fix" it by lowercasing identifiers at the comparison site. That is a
third comparator, agreeing with neither the index nor the column, and it will
diverge again the moment a non-ASCII identifier appears. The durable fix is to
constrain identifiers to a known alphabet at the API boundary and state the
resulting premise once for the package.
