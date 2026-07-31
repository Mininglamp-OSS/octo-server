---
type: Learning
title: "Grep for siblings before editing a duplicated block — approval/permission logic in this repo tends to exist in three copies, not two"
description: The invite-consumption failure block existed byte-identically in three Space approval entry points; the task brief named two, and fixing only those would have left a third approval surface behaving differently.
tags: [parity, duplication, space, approval, refactor]
timestamp: 2026-07-30T11:25:47+00:00
status: pending
---

# Count the entry points before fixing a duplicated block

## Context

`space-join-apply-resubmit` (#683) changed what happens when consuming an invite
code fails during join-application approval. The brief named two approval
entry points: the Space-scoped API (`api.go`) and the admin console
(`api_manager.go`).

There were three. The admin notification DM contains an `auth_code` link to an
H5 approval page, and `joinApproveSubmit` carries its own copy of the same
consume-fail block — byte-identical to the other two, right down to the rollback
log message. It surfaced only while resolving rules, when the `trust-boundary`
rule's *adapter parity* principle prompted a check for siblings.

Fixing the two named paths would have shipped an approval surface that still
reported "usage limit reached" for a disabled code and still left the applicant
un-notified — reachable from exactly the notification admins are most likely to
click.

## Learning

In this repo, a permission or approval decision reached from more than one
surface is usually **copy-pasted, not shared**. Before editing one copy, grep for
a distinctive line from the block (a log message or error code is ideal — they
are copied verbatim) and count the hits.

When there is more than one, extract a shared helper as part of the fix rather
than patching each site. Copies drift silently: nothing fails when one is
updated and the others are not, and the divergence surfaces later as
"it behaves differently when I approve from the email link".

## Applies to

Space join approval, invite-code consumption, and any flow reachable from both an
authenticated API and an `auth_code` / token link — the link-driven variant is
the one most often missed.
