---
type: Journal
title: "Learning: giving every project an all-member group, and what happens when the transaction stops being your serializer"
description: "The lock order pushed group provisioning outside the project transaction, which cost a CAS lease and made every ordering assumption explicit; five review rounds, including one where a fix reopened the thing it fixed and one where a test proved the statement ran while it planned as a full table scan; and the third repeat of one structural gap — a source guard that reads its subject file by name cannot see the file added next to it."
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
environment gap. A suite of end-to-end cases now runs against the real implementation
and assert on actual `group` / `group_member` rows.

### A process-wide latest-wins registry needs snapshot/restore in tests

The stand-in hooks were installed without restoring the real ones, on the
reasoning that the registry is latest-wins and every case installs its own. The
end-to-end cases do **not** install their own — they depend on the real hooks —
so an in-package case leaving a stand-in behind disabled the feature for every
later case in the binary. The symptom was the honest one: every one of them passed
alone and failed in a full run, which is what `-shuffle=on` exists to surface.
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

### Arguing a predicate away is not the same as showing it cannot fire

Scan B's banned-Space exemption was left out on the reasoning that no NEW gap can
open during a ban — every add and removal is refused while the Space is banned, so
nothing can go wrong that was not already wrong. That is true, and beside the
point: the exemption is not there to stop gaps appearing, it is there to stop the
gauge holding rows **nobody can act on**. A gap that predates the ban is still
reported, and its only documented repair goes through the path the ban refuses.

The more useful half is how the wrong answer was defended: with a test that opened
a gap AFTER the ban and asserted it was still reported. That checks the scan still
works. It does not check that what it reports is actionable, which was the whole
question. When a reviewer asks for an exemption and the answer is "the state
cannot arise", the test has to construct the state the exemption is about — not a
neighbouring one.

### The COLLATE rule had a second half nobody had written down

The repo's rule is "put the explicit COLLATE on the driving side's values, so the
driving table's indexes stay usable". True, and it hides what it costs: an explicit
COLLATE has coercibility 0, so the COMPARISON is in that collation and the OTHER
side must be converted per row — its index cannot serve it. In P0 and P1 that only
touched background scans. P2 put the same shape on `pkg/project.IsAllMemberGroup`,
which is the entire D7 guard, reached from six user-facing handlers: measured on
MySQL 8.0.46 it planned as a full scan of `group` on every group exit.

Two fixes were available and they are not equivalent. Moving the COLLATE to the
legacy side restores the plan **today** and becomes silently wrong the moment the
collation conversion lands, in the mirror direction. Splitting the join into two
single-table reads has no cross-schema comparison at all, needs no collation
opinion, and survives the conversion untouched. Where a join must stay — the paged
reconcile scans — the honest move was to correct the comment that asserted a plan
the production shape does not give, and leave the item in `open_verification`.

CI cannot see any of this: the test database is created with `general_ci` on both
sides, so every one of these plans is `const` there. The migration header P0 wrote
says exactly that — *"a green suite is structurally not evidence"* — and it took a
reviewer running EXPLAIN against a deliberately drifted database to make it
concrete.

### A justification can be true when it is written and false three rounds later

The migration justified its one index with "scan A's predicate is
`(status, all_member_group_no)`". That was accurate. Two rounds later the
flag-over-base-page fix moved `all_member_group_no` out of the WHERE and into the
violating flag — the right change, made for a different reason — and the index's
entire reason for existing left with it. Nobody noticed, because nothing about the
index changed.

Measured afterwards, neither scan chooses it under either collation shape: scan A
pages by `p.id`, which only the PRIMARY KEY can serve, and scan B is not driven
from `octo_project` at all. The same thing had already happened once in this PR to
the *other* index, and to the comment claiming the D7 join was "one index dive".

The pattern is that a predicate does not move alone. Its index, the comment
explaining the index, and the acceptance line quoting the comment all belong to
it, and moving the predicate without them leaves three artefacts asserting a fact
that stopped being one. Worth asking, whenever a WHERE clause changes: what was
justified by the shape it used to have?

### A test that proves a statement RUNS does not prove it runs well

The collation drift test was built for exactly this class of defect and could not
see the defect. It executes every cross-schema statement against a deliberately
drifted database, which answers the 1267 question — does this raise an error in
production. The fifth review's blocking finding was that the statement ran fine
and *planned* as a full scan of a core table on every group exit.

Two different questions, and the one the suite could answer was the cheaper one.
CI could never have found the second: its database is general_ci on both sides, so
every plan there is `const`.

What closed it is smaller than it sounds — `EXPLAIN` the same statements against
the same drifted fixture, and read the access type. The part worth copying is the
**control**: the guard first EXPLAINs the joined form that was removed and requires
that it IS a full scan under drift, and is NOT one after the conversion. Without
that, "type is not ALL" would pass on an empty table, on a fixture that stopped
drifting, on a statement nobody runs. With it, the assertion that the fixture can
still produce the bug is part of the test — and the same pair of assertions is the
argument for why the join was split rather than re-collated, held in CI instead of
in a comment.

### A step that removes alternatives has to be able to defend the one it keeps

The fifth round made "two `role=creator` rows" a state to repair: pick a keeper,
demote everyone else. The demote loop was correct. The keeper choice had an arm
where it was not — the pool-picked successor is the *senior* project owner, and
when that person is not in the group the promotion falls through and the keeper
stayed the senior creator, who had already been established as a non-owner. If a
*junior* owner held one of the other creator rows, the convergence then demoted
the only valid owner the group had.

The state it produced was worse than the state it was fixing. Before, the input
wrote nothing and left two creators, one of them valid. After, one creator, not
valid — and unreachable: D7 refuses transfer, exit and disband, the sync only
re-fires on a project owner change, and no scan asks whether a group's creator is
a project owner.

The lesson is not about owners. Adding a step that *deletes* alternatives changes
what every earlier branch is responsible for: each one now has to justify the
survivor, not merely name a default. The two tests written with the fix both
exercised arms where the default happened to be right, which is the ordinary way
this gets missed — the arm with no case was the arm that misbehaved.

### A guard whose subject is named in the prose beside it is not a guard

Fourth variation on the same theme in this change, and the sharpest. The
thread-subscription fix was pinned by a source guard matching the token
`addUsersToGroupThreads` in the function body. The doc comment two lines above the
call names the function, so the reviewer deleted the call — nothing else — and the
guard stayed green.

The earlier variations were about the guard's *file list* (a guard that names its
subject file cannot see the file added beside it) and its *inputs* (an enumeration
guard is only as good as the paths it drives). This one is about its *haystack*:
source text contains both the code and the writing about the code, and a match
against the raw bytes cannot tell them apart. Strip comments, and match a call —
receiver and open paren — not a name.

Worth noting what did work. Two other source guards in the same commit were
mutated by the same reviewer and both went red, because both matched call shapes.
The technique is fine; the haystack was not.

### Counting occurrences pins the file's size, not the thing you meant

The round-6 fix for "the plan guard EXPLAINs constants, so production could stop
running them" was a guard requiring each constant to appear at least twice in its
file — a declaration plus a use. It was vacuous on arrival, and the reason is
almost funny: the third reference is the exported test helper *the plan guard reads
the constants through*. Delete the production use and the count is still two. The
reviewer proved it by re-inlining the exact joined query the guard names, and
everything stayed green.

The module side was soft for a different reason — two production call sites each,
so deleting one still left two — which is the tell. A threshold has to be re-derived
every time the file changes, and nobody re-derives it.

Body-scoping is the instrument that does not have this property: name the function
that must run the statement, slice its body, strip the comments, and look inside.
Then each call site is pinned individually and no reference anywhere else can
satisfy it. Same move as the round-6 comment-stripping fix, one level up — and this
is the fifth time in this change that a guard turned out to be matching a haystack
larger than its subject.

### A rule is only as done as its entry-point census

D14 says bot deletion must go through the Space removal outbox rather than a bare
`UPDATE`. It was implemented, tested, mutation-pinned, blocked on once and fixed —
on the chat-command path. Bots have two deletion entry points. The REST endpoint
kept doing exactly the bare update the decision forbids, through seven review
rounds and every self-review, because nobody asked "what else can delete a bot".

The residue it leaves is the worst kind: a disabled bot still holding an active
project seat and an active group membership, with no witness. I1's scan is
default-off and waiting on the collation conversion; I4's scan B looks for a member
*missing* from the group and this ghost has both rows; D13 cannot reclaim a seat
whose robot row is disabled; and the deletion cannot even be retried, because the
endpoint's own lookup requires the row it just disabled.

The discipline that would have caught it is one this change applied well in three
other places — D7's refusal placed across every handler that mutates membership,
the admission-entry constants compared against the guard lists, the five removal
shapes enumerated by effect rather than by name. It was never applied to the
subject of D14 itself. When a decision names a behaviour ("deletion must…"),
the first artifact is the list of code paths that perform it — and that list
belongs in the acceptance criteria, not in someone's head.

### Fixing the instance you were shown is not fixing the defect

One shape — an explicit `COLLATE` written on the pinned side of a comparison
against a legacy table — was found and fixed three times in this change, in three
different statements, across three review rounds. Each round fixed the instance it
was handed. Nobody enumerated the others, so the third and worst instance was
still there after two rounds of "fixed": a correlated subquery on the project LIST
route, paying the cost once per listed project, on a route with no feature gate in
front of it.

The reviewer's process note is the lesson, and it generalises past collations:
after the second instance of anything, the next move is a census, not a fix. Two
of them were owed here — every statement in the diff comparing a pinned column
against a legacy table, and every entry point that can close a `space_member` row
or delete a bot — and both are mechanical enough to be tests rather than reviewer
attention. The second one had already cost a blocking finding a round earlier, for
exactly the same reason.

The countermeasure that stuck is not a rule, it is coverage: the statements now
live in the drift suite (do they execute) and in the plan guard (do they keep an
index), so the *class* is watched rather than the instances someone happened to
notice.

### Measuring beats arguing, and the fixture is part of the measurement

The last cross-schema join on a write path drew a plausible argument that it
degrades — it runs under `FOR UPDATE`, so a scan there costs lock-hold time, not
just latency. Measured, it does not degrade: the entry point is a literal
`creator_uid` predicate, and the join lands on an index at both ends.

The first measurement said the opposite, and the difference was the fixture: a
probe where one user owned every robot made `idx_robot_creator_uid` perfectly
unselective, and the plan degraded exactly as feared. Redistributing the same 2000
rows over 200 owners flipped it. Both numbers were real; only one was about
production.

So the comment was still wrong — it credited `robot`'s primary key, which the
COLLATE does make unusable — and the honest fix was to say what carries the join
and pin the plan, rather than to restructure a locking read on an argument.

### "Does the token appear" and "is the token in the right place" are different guards

The last two holes found in this change were the same mistake at two scales. A
guard asserted that `return errGroupGoneOrDisbanded` appeared somewhere in a
function — and the function contains that line twice, because the same answer is
given at two points. Deleting the one that mattered left the guard green.

Every source guard here has now been bitten by the size of its haystack: the file
list (a guard that names its subject file cannot see the file added beside it),
the inputs (an enumeration guard is only as good as the paths it drives), the text
(prose beside the code satisfies a token match), the file scope (a constant
referenced by the test helper satisfies an occurrence count), and now the position
(a token that appears twice satisfies a check meant for one of them).

The shape of the fix has been the same every time: narrow the haystack to exactly
the thing being asserted. Slice the function, not the file. Strip the comments.
Match the call, not the name. Assert the window between two landmarks, not the
presence of a string. None of these is clever; what they have in common is that
the assertion and the property finally have the same boundaries.

### A census is only as good as the question it asks

The bot-deletion census ran twice and missed the same door twice, and both times
the reason was the question, not the effort. It was run as "who WRITES
`space_member`" — which cannot, structurally, find a door whose entire defect is
that it deletes a bot WITHOUT writing `space_member`. The right question is "what
deletes a bot", and it has a different answer: three doors, not two.

The correction is not "be more careful next time". It is that a census is a
predicate over a set, and if the predicate is derived from the fix rather than
from the rule, it enumerates the places already fixed. The rule here is "a
deleted bot's Space seats close through the outbox"; its subject is bot deletion,
so the enumeration has to start from deletion, not from seat writes.

What made it stick is that the census is now a test over the SET, not three tests
over three endpoints: it finds the primitives that turn a bot's account off, finds
their callers, and requires each caller to route or to be a recorded exemption.
Written that way it immediately produced a fourth call site nobody had named — a
creation-rollback path that is correctly exempt. An endpoint-level test could not
have surfaced that, because you cannot write a test for a door you do not know
about.

### When ordering is undeclared, do not compute an answer that depends on it

The Space-removal cascade runs its cleanup steps in registration order, and
registration order is import order, which is declared nowhere. That was already
recorded as the reason a failing step must not block the others. It has a second
consequence that this round found: some answers are only correct once every step
has run, and computing them from inside a step gives an answer about whichever
intermediate state the ordering happens to produce.

Concretely, "the all-member group's owner must be an active project owner" needs
both the group handover and the project seat close to have happened. Place the
sync in the project step and the two possible orders fail differently — one lets
`PickActiveOwner` return the person on their way out, the other syncs before the
handover installs the wrong owner. Neither order is a bug in the ordering; the bug
is asking a question whose answer is not yet defined.

The fix is a phase, not a position: a post-steps finalizer that runs only after
every step has succeeded. It is worth noticing that this is not "a fifth step with
a good name" — the registry cannot promise a step's position, and a comment asking
future maintainers to register in a particular order is not a mechanism.

### A test whose fixture has a tie is asserting a coin flip

The end-to-end case for the above passed with the fix removed. `created_at` on
`group_member` is second-granular, both members were added inside the same second,
and the successor query orders by `created_at` with no tie-break — so which member
the handover picked was whatever the storage engine returned first, and it
happened to be the right answer.

Every previous round of this change found a guard that asserted less than it
claimed; this one is the same failure in the fixture instead of the assertion. The
mutation run is what caught it, again, and it is the only thing that could have:
the test passed, its name was right, and its comment described a property the setup
did not actually create. Backdating one row by an hour is the whole fix, and the
lesson is that a fixture which makes the wrong answer and the right answer equally
reachable proves nothing about which one the code picks.

### "It survives" and "it is affordable" are different questions

Both I4 scans were kept outside the reconcile gate on the argument that every
cross-schema comparison in them carries an explicit `COLLATE`, so they survive the
collation drift the gate exists for. That argument is true, was tested, and was
beside the point: the same file already recorded their measured plans under the
production shape — a full scan of `group` plus a temporary table, and the temporary
table defeats the `ORDER BY`/`LIMIT` paging the scans depend on, so `ReconcileLimit`
stops bounding the work.

Surviving `1267` says the statement executes. It says nothing about executing it
every five minutes on every pod against a core IM table. The two scans now ride the
same switch as the three that cannot survive the drift, for a different stated
reason, so the next reader does not "fix" the inconsistency by moving them back.

### A guard passing for the wrong reason cannot expose its own blind spot

The bot-deletion census kept a list of primitives it must keep recognising, precisely so
the matcher could not rot into a test that asserts nothing. `deleteCreatedBotArtifacts`
was on that list and was found on every run — and the matcher was still blind to the
spelling that function actually uses. It uses `DeleteFrom("robot")`, which the matcher
did not know; it registered as a primitive only because of an unrelated raw `UPDATE` in
its fail-closed fallback.

So the anti-vacuity check was green, and green for a reason unrelated to the thing it was
protecting. That is a failure mode worth naming separately from vacuity: an assertion can
be non-vacuous, correct, and still incapable of detecting the defect it was written
against, because the sample it holds up is passing through a different code path than the
one under test. The check for it is the same one that keeps catching things here — mutate,
and mutate with the shape you claim to cover, not the shape you already handle.

### Classifying a thing wrongly can make a fix pass its own mutation

The fix for the second census bypass — a disable spelled across two functions — was
written first as "the caller is a primitive". It was, in a sense. It also meant the caller
was skipped by the rule that a primitive is not a door onto itself, and since nothing
called it, nothing was asserted about it. The mutation stayed green against the fix
written for it.

The correct classification is that the caller is a **door**: in that shape the deletion
site is the function holding the literal, because the `UPDATE` it borrows lives in a
generic helper that is not itself a deletion. Same code, same coverage, opposite outcome
— the taxonomy was load-bearing and looked like naming.

### "Same retryable error" is a claim about the input, not about the error

The Space-removal finalizer returned the cascade's retryable error when its page budget
ran out, and said so in a comment: same error, same meaning. The error was the same. The
meaning was not, and the difference is the only property that makes a retry worth asking
for.

The cascade re-queries *active* seats, so every row it closes leaves its own result set:
each retry starts from a smaller input and the walk terminates. The finalizer's query
deliberately has no status filter — with one it returns the empty set and converges
nothing — so a retry re-reads the same first page forever, and the job spends its whole
attempt budget on a walk that cannot advance, dragging every already-successful step
through the re-runs with it.

Two call sites can share an error value, a retry mechanism and a page budget and still
disagree about whether retrying does anything. The question to ask of any "retry later" is
not what it returns; it is what will be different next time.

### A non-locking read narrows a window; it does not close one — and the obvious fix was a deadlock

An in-transaction re-read was added to make the owner sync judge attribution from an
authoritative snapshot. It reads into the transaction's read view, which means a write
committing afterwards is invisible to it — and *invisible* is not *blocked*. The
subsequent `group_member` locks do not revalidate the `group` row, so the sync proceeds on
an attribution that is no longer true.

The distinction is easy to lose because both readings sound like isolation. "I will not
see a later write" protects you from a torn view of the past. "A later write will wait for
me" is what protects a decision you are about to act on. Only the second one was wanted
here, and taking a shared lock on the `group` row looked free: it sits exactly where the
declared lock order already puts it.

It was not free, and the reason is the more useful half of this entry. P1's cascade takes
`group_member` first and then, on its no-successor branch, an exclusive lock on the same
`group` row — `group_member` then `group`. A locking re-read here is `group` then
`group_member`. Both paths reach the same all-member group (the cascade's detach branch
needs "nobody in the group is still in the project", which a one-person project satisfies
the moment its owner leaves), so concurrently that is ABBA and MySQL kills one side.

So the lock was reverted and the window written down instead — with what it costs (a
Space-direct group with a possibly-wrong creator, repairable by an ordinary transfer,
because Space-direct means D7 no longer applies) and what has to happen first (the
cascade's inversion of the declared order is the actual defect; straightening it is a
change to a load-bearing path and deserves its own analysis).

Two things worth keeping from this. **"It is in the declared lock order" is a statement
about one call site, not about the system** — the order only holds if every path obeys it,
and the one that did not was the reason. And **a fix that survives the full suite can still
be wrong**: nothing here failed. It took reading the other path's lock sequence to see it.

### "Is this breaking?" and "is this right?" are different questions, and the first one can hide the second

`member_count` was flagged as the one change in this feature that would visibly break
clients: redefined from "everyone" to "humans only", so any project with agents renders a
smaller number. The whole discussion was about blast radius — how many clients read it,
what the fallback costs, whether to measure it first.

The blast radius turned out to be zero. The module has never been GA, so nothing had
shipped against either meaning. Under the question as asked, that is the end of it: not
breaking, no action.

Asking the second question found something the first could not. This server already ships
`member_count` — meaning the total — beside `human_member_count` and `agent_member_count`,
from a different module, to the same client teams. So the change was not breaking; it was
*inconsistent*, and inconsistent in a way that fails silently: both fields are ints, both
compile, and a client author who learned the convention from one module renders the wrong
number in the other. No test on either side can catch it, because neither side is wrong on
its own.

The fix was the fallback that had been designed for the breakage that did not exist —
restore `member_count`, add the two split fields — adopted for an entirely different
reason. Worth noticing: had the first question come back "yes, breaking", the same fix
would have landed and the naming collision would have gone unnoticed, because the answer
would have arrived before anyone looked at what the field ought to mean.

The original argument for the change was sound and remains in the code: a project with one
person and two of their agents should not render "3 人". That is a claim about what a
client should DISPLAY. It was answered by changing what the server MEANS. Those are
adjacent enough to substitute for each other without anyone noticing, and the substitution
costs a name.

## What we did not deliver

- **No automatic repair for I4.** Both scans report only. Scan A's repair lives on
  the write paths (the lease-claimed rebuild); scan B has none, by decision,
  because repairing it means writing `group_member` from the worker whose job is
  to be the invariant's witness. The documented operator action — re-add the
  member — is now actually wired: it used to be gated on the seat write having
  changed something, which is false precisely in the state the gauge reports.
- **The all-member group's threads.** The admitter subscribes the new member to the
  parent channel and, since the fifth review round, to the group's threads. Nothing
  reconciles thread subscriptions: I4 scan B reads `group_member` rows, and a
  member who is in the group but missing from a thread's subscriber list is
  invisible to every scan here. The pairing is held by a source guard, not by a
  behavioural test, for the same reason as the item below — the broker's subscriber
  table cannot be read back.
- **Only one direction of I4 is policed.** The invariant is written as an
  equality; both scans measure "an active project member missing from the group".
  Extras are reachable today through the repo-wide system-bot exemption — the
  admission gate waives I2 for whitelisted bots and `memberAdd` is deliberately
  not D7-guarded — and nothing reports them. Narrowing the exemption here would
  give this one group a different bot rule from every other group, so it stays;
  what is worth saying plainly is that a second source of extras would go
  unreported too.
- **The all-member group's IM subscriber list is not asserted to equal its member
  set.** Nothing in octo-server or octo-lib can read a channel's subscribers back
  from the broker, so the equality is not assertable from a test. Same gap P1
  recorded for its stale-subscriber scan, and it should close with whichever
  change adds that capability.
