---
type: Journal
title: "member_epoch 0 was both a real value and the absent sentinel"
description: A review blocker on PR #852 — the epoch a fresh project carries was the same value the integration contract reserves for a project that does not exist, so a consumer could cache an authorization grant that never expired.
tags: [project, auth, acl, wire-contract, data-integrity]
timestamp: 2026-09-08T00:45:00+00:00
---

# member_epoch 0 was both a real value and the absent sentinel

## What happened

PR #852 added two internal endpoints that report `member_epoch` to a peer
control plane, and documented in four places that `0` is fail-closed because it
"matches no real snapshot". Review found it does.

`member_epoch` defaults to 0, and `createProjectOnce` deliberately did not bump
it — creation was reasoned about as where the roster comes into existence rather
than changes. Measured rather than argued: a project created through the real
handler reads `member_epoch=0, status=1, active_members=1`. Meanwhile the
integration contract defines 0 as "the project does not exist or is not
visible", and the epoch query filters on `status = 1`, so a disbanded project
also answers 0.

Both directions fail:

- **Stale grant.** A consumer caches a positive authorization answer for a fresh
  project under epoch 0. The project is disbanded. The endpoint answers 0 for
  the now-absent project. `0 == 0`, so the staleness check AGREES, and the grant
  survives — permanently, because the epoch never moves again.
- **Availability.** A brand-new project truthfully reports 0, which a consumer
  following the contract reads as "does not exist" and denies.

## What was done

Fixed at the source, not on the wire: creation now bumps the epoch, so a new
project lands on 1 and 0 becomes unreachable for a real project. A migration
increments existing rows still at 0.

Three wire-side alternatives were rejected, all because they renegotiate a
frozen contract and one reintroduces the bug on the other side:

- `null` for absent — the contract refuses to expand beyond a bare number, and
  `null` unmarshals into a Go `int64` as 0, so the Go consumer lands on the
  identical collision.
- a negative sentinel — still a contract change, and the sentinel still lives
  inside the value domain.
- omitting absent ids — a consumer cannot distinguish a missing key from an
  entry dropped in transit.

Starting at 1 is the only option needing no consumer change at all.

## Learnings

**A sentinel inside a value domain is only safe if the domain cannot reach it,
and "cannot reach it" has to be a property something enforces.** The contract
asserted 0 was unreachable; nothing made it so, and the code that made it
reachable was written for an unrelated reason a year of comments explains well.
The assertion and the invariant lived in different repositories.

**The first fix violated an existing guard, and the guard was right.** Seeding
`member_epoch` through the insert column list tripped `TestIsOfficialHasNoWriter`
("member_epoch may only be written as `member_epoch + 1`"). Satisfying the guard
instead of amending it produced a better fix: creation writes the owner seat,
which IS a membership write, so bumping is the consistent reading — the old
exemption was the anomaly. When a guard blocks a fix, the fix is usually the
thing that is wrong.

**Stub-backed tests could not have caught this.** They asserted the handler
faithfully returns whatever epoch the store hands it, which it did. The defect
was in the choice of sentinel, so the assertion had to be against a real row in
the module that owns the column.
