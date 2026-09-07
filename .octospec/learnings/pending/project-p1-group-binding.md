---
type: Learning
title: "An explicit COLLATE goes on the driving side, and it cannot replace a feature gate"
description: "When two schemas disagree on collation, an explicit COLLATE only stays cheap if it lands on the driving row's values; when the pinned table drives the join, coercing the legacy side destroys its index and a gate is still required."
tags: ["mysql", "collation", "performance", "reconcile", "feature-flag"]
timestamp: 2026-09-07T02:00:00Z
source: self
status: pending
---

# An explicit COLLATE goes on the driving side, and it cannot replace a feature gate

## The rule

Two rules, and the second is the one that gets missed.

**1. COLLATE goes on the LEGACY side of a legacy ⟷ pinned comparison, and
nowhere else.** Not on a legacy ⟷ legacy join — both sides already agree there,
and forcing one makes the predicate non-sargable, so a "safety" measure costs a
full scan on the largest tables. And the rule is about which SIDE a column is on,
not which module wrote the query or created the table: a table created BY a
migration with an explicit collation sits on the pinned side even when it belongs
to a legacy module. A comparison in the SELECT list raises 1267 exactly as one in
an ON clause does.

**2. COLLATE is only cheap when the coerced column belongs to the DRIVING row.**
Coercing a column always makes it non-sargable. That is free when the column is
the driving row's value (a constant per row) and the looked-up table keeps its
index; it is catastrophic when the coerced column is the one the lookup needs an
index on.

So whether a cross-schema statement can be fixed with COLLATE — or needs a
feature gate until the collation is unified — depends on which table drives:

| driving table | looked-up table | coerce | cost |
|---|---|---|---|
| legacy | pinned | the driving row's values | free; pinned index still used |
| pinned | legacy | the legacy column | full scan of the legacy table, per row |

And the escape hatch does not exist: naming the LEGACY collation instead would
work while drifted and raise 1267 the moment the conversion lands, so the
comparison collation has to be the pinned one either way.

## The measurement

MySQL 8.0.46, deliberately drifted schema (legacy tables `utf8mb4_0900_ai_ci`,
module tables `utf8mb4_general_ci`), 50k `space_member` / 5k member rows,
`EXPLAIN` on a statement that drives from the PINNED table:

```
drifted   + COLLATE ->  sm  type: ALL     50118 rows scanned PER EXAMINED ROW
converged + COLLATE ->  sm  type: eq_ref  1 row, index spacemember_spaceid_uid
```

The same COLLATE is free after the conversion and ruinous before it. A scan that
reads 1000 rows a page, 25 pages a tick, every five minutes cannot pay that.

## Why it matters beyond one query

It settles an argument that otherwise runs on intuition: *"we added COLLATE, so
the compatibility flag can go."* Sometimes yes, sometimes no, and the answer is
decided by the join's driving side rather than by whether the error is gone.
Removing the flag on the strength of "it no longer raises 1267" trades a loud
failure for a silent one.

## How to check it, mechanically

Build a probe database, `CREATE TABLE ... LIKE` from the real schema so the
fixture cannot drift, `CONVERT TO` the legacy half, populate enough rows for the
optimizer to be honest, and `EXPLAIN` the real query method — both drifted and
converged. Asserting only "it does not error" is what let two COLLATE mistakes
through in this task; asserting the access path is what caught them.
