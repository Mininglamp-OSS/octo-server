---
type: Learning
title: "A column's migration lives where every reader can see it"
description: "Place a schema change in the migration directory of a module that every reader imports — not the module that conceptually owns the column."
tags: ["migration", "testing", "module-boundary"]
timestamp: 2026-09-06T18:00:00Z
source: self
status: pending
---

# A column's migration lives where every reader can see it

## The rule

When a column is read or written by more than one module, it must ship from the
migration directory of a module that **every** reader imports. Not from the
module that conceptually owns the concept.

Check it mechanically before choosing:

```
go list -deps ./modules/<reader> | grep octo-server/modules/<candidate>
```

for every reader. If any reader comes back empty, that candidate is wrong.

## Why it is not obvious

Each module's test binary applies only the migrations of the modules it links.
So a column placed in a directory a reader does not import simply does not exist
in that reader's tests, and the failure is `Error 1054: Unknown column` across an
entire package — far from the placement decision that caused it.

The tempting rule, "the module that owns the concept owns the column", gives the
right answer often enough to look correct: it agrees with the dependency rule
whenever the owning module happens to be a dependency of every reader. It is the
cases where it disagrees that cost a debugging session.

## Worked example

`group.project_id` is read by modules/group (the admission gate), modules/space
(preset-group validation) and modules/project (the reconcile scans).

| binary          | has space migrations | has group migrations | has project migrations |
|-----------------|----------------------|----------------------|------------------------|
| modules/group   | yes                  | yes                  | yes (P1 only)          |
| modules/space   | yes                  | no                   | no                     |
| modules/project | yes                  | no                   | yes                    |

Only `modules/space/sql` is in all three. Two other placements were tried first
and each took out a whole package.

This is also why `group.space_id` was added from modules/space's own directory in
2026-03 — a precedent that has been cited for the ownership rule, which it does
not actually support.

## Where the rule does NOT apply

Schema only one module touches stays with that module. In the same change,
`octo_project_member.removing` and the cascade outbox correctly stayed in
modules/project/sql, because nothing else reads them.
