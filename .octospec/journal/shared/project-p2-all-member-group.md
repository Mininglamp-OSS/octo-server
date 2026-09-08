---
type: Journal
title: "Learning: giving every project an all-member group, and what happens when the transaction stops being your serializer"
description: "The lock order pushed group provisioning outside the project transaction, which cost a CAS lease and made every ordering assumption explicit; three review rounds where a fix reopened the thing it fixed; and the third repeat of one structural gap — a source guard that reads its subject file by name cannot see the file added next to it."
tags: ["octospec-learning", "space", "isolation", "acl", "wire-contract", "migration", "testing", "reconcile", "mysql"]
timestamp: 2026-09-08T02:30:00Z
source: self
---

# Learning: project-p2-all-member-group

## What changed

Creating a project now seats the creator's own AI agents in the same transaction
and provisions an **all-member group** whose active member set equals the
project's active member set. That equality is invariant **I4**, and it is the
whole of this change: everything else is machinery keeping it true.

- Membership follows in both directions — an add admits into the group (D12), a
  removal is already carried by P1's detach cascade, and a departing member's own
  agents lose their seats with them in the same transaction and the same single
  epoch bump (D13).
- Five group-face operations — disband, exit, member removal, owner transfer,
  **blacklist** — are refused on an all-member group (D7), in the HTTP handlers
  only.
- The group's owner tracks the project owner (D6) and its name tracks the project
  name (D8); disbanding the project releases the group back to Space-direct.
- Two reconcile scans report the two halves of I4; neither repairs anything.
- `modules/project` still does not import `modules/group`: group registers four
  hooks into project, the same reverse-registration P1 established.

## What we learned

### When the ordering constraint moves work outside the transaction, the transaction stops being your serializer

The declared lock order is `space_member → space → project → group →
group_member → octo_project_member`. Everything the group hooks do touches
`group` and `group_member`, so they must run **after** the project transaction
commits — otherwise they take those locks under the project row lock and invert
the order for every concurrent group write.

That single constraint decides the shape of the whole feature. The project row
lock is gone by the time the provisioner runs, so it cannot serialize two
concurrent rebuilds; a **CAS lease on a column** (`all_member_group_lease_until`)
had to take its place. Every "the transaction protects this" instinct is wrong on
the far side of that commit, and each one has to be replaced explicitly.

The check that keeps it true is worth more than the argument: the stand-in
provisioner reads `octo_project` on a **pooled connection**, where the row is
visible only if the create transaction is already gone, and every stubbed case
carries it. That assertion exists because the Verify phase went looking for it
and found the brief had asked for it and nobody had written it.

### A protection added for an invariant can kill the cascades that maintain it

D7 refuses five group operations. Putting the refusal in the service-layer
primitives — the obvious place, one guard covering every caller — would have
blocked P1's member-removal detach, the Space removal cascade, botfather's bot
deletion, and P2's own owner-sync hook. That is: the guard added to maintain I4
would first disable the code that maintains I2 **and** I4.

The refusal belongs to "a person clicked this button", not to "the system is
maintaining an invariant", and those two live at different layers. A source guard
(`TestAllMemberGroupProtectionIsNotInTheServiceLayer`) now stops it being pushed
down.

The corollary is that every handler calling a primitive directly needs its own
copy — which is how `modules/bot_api`'s member removal was found still open after
the Web side was closed.

### The fifth path did not call itself a removal

Four operations were obvious. **Blacklist** was not: it is not named "remove", it
does not go through `RemoveGroupMembers`, it just flips `group_member.status` and
unsubscribes. Its effect on I4 is identical to a kick. P1 reached the same
conclusion about the same path when enumerating admission entries — *it is that
thing, so treat it as that thing* — which is the argument to reach for whenever a
path is classified by its name instead of its effect.

### A guard that names its subject file cannot see the file added beside it

Third occurrence, and this time it let the defect back in.

`TestReconcileQueriesAreBounded` and the flag-over-base-page cost guard both read
`reconcile.go` by name. P1 added `reconcile_p1.go`, discovered neither guard
covered it, and wrote `reconcile_p1_bounds_test.go`. P2 added `reconcile_p2.go`
and shipped `g.id IS NULL` in a WHERE clause — the exact predicate the cost guard
forbids, for the exact reason it forbids it: in a **healthy** database that
predicate matches nothing, so `LIMIT` bounded rows *returned* rather than rows
*examined* and the scan walked the whole of `octo_project` every tick. The cost
was highest precisely when there was nothing to report.

Two contrasts are worth keeping:

* `TestGroupNoLegacyResponseError` **discovers** its files from the directory,
  and it needed no maintenance across P0, P1 and P2.
* Everything that enumerates by hand — the two admission-entry lists, the
  migration file list, the collation probe tables — needed an entry this round,
  and each was a place the round could have silently shrunk the guarantee.

Discovery beats enumeration wherever the set is derivable. Where it is not, the
enumeration needs its own guard: `TestEveryAdmissionEntryConstantIsInTheGuardLists`
reads the constants out of the source and compares, so the next entry cannot be
forgotten the way this one nearly was.

### An enumeration guard's coverage is defined by its own inputs

`TestEveryWritePathEmitsAnAuditEntry` was green the whole time the create path
wrote agent seats that emitted no audit entry at all — because the test never
sent `agent_uids`. Same for `TestWritePathsRevalidateTheActorSpaceSeatInTx`,
whose stated scope was "the callers of `requireSpaceSeatsTx`" while
`createProject` takes its `space_member` locks directly and was therefore outside
it by construction.

A guard that passes on the paths it already knows about is not a floor. When a
change adds a path of a kind some guard enumerates, the registration is part of
the change, not follow-up.

### A fix that tightens one predicate must tighten the one that answers the same question

Review round 1 made `queryAllMemberGroupNo` verify the group side — the group
must exist, not be disbanded, and still carry this project's id. Correct. The
**claim CAS** was left keyed on the pointer being the empty sentinel, and P1's
detach leaves the pointer set when it releases a group.

So the lookup said "no group" while the claim said "already has one". The rebuild
never ran, every later admission no-opped, scan A reported a project nothing
could repair — with no error and no metric anywhere. Two predicates disagreeing
about one fact, which is the same shape as the bug round 1 was fixing.

### A promote-then-demote is only safe if the demote is conditional on the promote

`ensureAllMemberGroupOwner` read the successor's `group_member` row without a
lock, promoted with a helper that silently affects zero rows on a soft-deleted
row, then demoted the sitting creator unconditionally. A concurrent soft-delete
of the successor's row between the two loses **both**, and D7 then makes the
resulting ownerless group unfixable from the group side.

The repo already had the answer two hundred lines away: `project_cascade.go`
locks the successor `FOR UPDATE`, promotes with the live-row variant, and hard
fails if it did not land. The lesson is not "lock the row" — it is that a handover
written as two independent statements has a state between them, and the second
statement has to be told about the first one's outcome.

### "This cannot be tested end to end" was wrong, and the requester was the one who said so

The claim was that the seam between `modules/project` and `modules/group` could
not be exercised because project must never import group. The import ban
constrains the **package**, not the test **binary**: the external test package
already blank-imports `octo-server/internal`, so `module.Setup` registers
modules/group's real provisioner, admitter, owner sync and rename in that same
binary.

The claim also conflated two obstacles — an architectural rule that does not
apply, and a missing WuKongIM broker in the dev environment, which is an
environment gap. Eight end-to-end cases now run against the real implementation
and assert on actual `group` / `group_member` rows.

### A process-wide latest-wins registry needs snapshot/restore in tests

The stand-in hooks were installed without restoring the real ones, on the
reasoning that the registry is latest-wins and every case installs its own. The
end-to-end cases do **not** install their own — they depend on the real hooks —
so an in-package case leaving a stand-in behind disabled the feature for every
later case in the binary. The symptom was the honest one: all eight passed alone
and failed in a full run, which is what `-shuffle=on` exists to surface.
`modules/space`'s removal-step registry carries the same warning; this now
follows it.

### Anti-enumeration is about the reason, not the subject

Six ways an agent can be ineligible collapse to one error code, and the specific
reason goes to logs only — the endpoint must not become an oracle for "does this
uid exist / is it a bot / who owns it".

The first implementation drew the wrong conclusion from that and echoed **every
submitted uid** back in the details, on the argument that echoing the caller's own
input leaks nothing. True, and beside the point: one bad uid in a batch of ten
told the client all ten were refused, so the user's only recovery was to re-pick
from scratch. Hiding *why* and naming *which* are independent decisions; conflating
them cost precision for no privacy.

### Ordering an authorization check against a type check builds or avoids an oracle

`addOneMemberOnce` first asked "is this an agent, and may the actor seat it?" and
only then applied the permission gate. An ordinary member could then tell a live
bot owned by someone else (a per-uid `agent_not_eligible`) from a human or an
unknown uid (an actor-level `permission_denied`) — exactly the distinction the
single code exists to hide, handed to the least privileged caller there is.

Decide **ownership** first (it is the only thing an ordinary member may act on),
then the permission gate, and let the type-specific refusal become visible only
to a caller who already passed it.

## What we did not deliver

- **No automatic repair for I4.** Both scans report only. Scan A's repair lives on
  the write paths (the lease-claimed rebuild); scan B has none, by decision,
  because repairing it means writing `group_member` from the worker whose job is
  to be the invariant's witness. The documented operator action — re-add the
  member — is now actually wired: it used to be gated on the seat write having
  changed something, which is false precisely in the state the gauge reports.
- **Scan B does not exempt banned Spaces**, though the brief listed it. P1's I2
  scan needs that exemption because the Space cascade deliberately leaves those
  seats alone and the group rows are *expected* to remain — that is the ⊆
  direction. Scan B is ⊇, and the state it would suppress cannot arise: the
  cascade leaves both sides alone, `removing = 1` already exempts the closing
  case, and every path that could strip a group row without touching the seat is
  now refused by D7. The exemption would be a `space` lookup per examined row that
  can never fire.
- **The all-member group's IM subscriber list is not asserted to equal its member
  set.** Nothing in octo-server or octo-lib can read a channel's subscribers back
  from the broker, so the equality is not assertable from a test. Same gap P1
  recorded for its stale-subscriber scan, and it should close with whichever
  change adds that capability.
