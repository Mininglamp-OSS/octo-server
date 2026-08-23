---
type: Task
title: "Task: space-removal-creator-handover-notice"
description: When a Space removal takes out a group's creator, tell the group who took over and why. An ordinary member's removal stays silent on purpose — the roster already shows it, and broadcasting it would put up to 10k permanent messages into groups per batch.
tags: [space, isolation, wire-contract, external-content, escape, concurrency, testing, commit]
timestamp: 2026-08-23T11:00:00+00:00
# --- octospec extension fields ---
slug: space-removal-creator-handover-notice
upstream: follow-up to Mininglamp-OSS/octo-server#795
source: user
---

# Task: space-removal-creator-handover-notice

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. Examples and tests use synthetic UIDs and Space IDs only.

## Goal

When removing a member from a Space cascades them out of a group, the members
still in that group must be able to tell what happened to the group **when the
change is not otherwise visible to them**.

Exactly one case qualifies: the removed member owned the group. The cascade hands
the group to the second-oldest non-bot member, and before this task that happened
in complete silence — the group acquired a new owner with nothing explaining it.
A departure, by contrast, *is* visible: the roster shows one fewer person.

So this task ships one group-visible message, on one condition:

> `“{0}”已离开当前空间，“{1}”已成为新群主`

sent to the whole group when, and only when, a Space removal caused a creator
handover. Both clauses are needed: the group is seeing the consequence
(ownership moved), and without the cause it reads as an unexplained change.

### Explicitly not in this task

An earlier revision of this branch also broadcast a per-member departure notice
(`X 已被移出当前空间…`) into every group. That is **deliberately not shipped** —
see Out of scope for the reasoning, and do not re-add it without revisiting the
volume argument.

## Background

### What #795 left behind

`#795` (`space-member-removal-cleanup`) made Space removal cascade into the
Space's groups. Reported symptom afterwards, and the origin of this task:

> 相比没有 795 之前会清理群聊了, 但是没有系统消息

Measured against the CI-pinned stack by recording the `/message/send` calls of a
kick cascade in a three-member group. For the two members who remained:

| # | type | content | subscribers | visibles |
|---|---|---|---|---|
| 0 | 1020 | `你被{0}移除群聊` | `[victim]` | `[victim]` |
| 1 | 99 | *(none)* | – | – |

Message 0 is octo-lib's `SendGroupMemberBeRemove`, which pins **both** recipient
layers to the removed member and is written in the second person — by design a
private notice to the person being removed, not a group broadcast. Message 1 is
`CMDGroupMemberUpdate`, a silent roster refresh with no visible content. Net
group-visible messages for the remaining members: **zero**.

octo-lib does have a third-person, group-visible `SendGroupMemberRemove`, but it
fires only from the `GroupMemberRemove` event and nothing in this repository ever
emits that event.

### Relationship to #795's brief

`space-member-removal-cleanup` brief, Acceptance › Group cascade, states:

> Each affected group receives a member-update CMD and a system tip so remaining
> members' clients converge.

**This task supersedes the "and a system tip" half of that line.** The tip that
line refers to is `SendGroupMemberBeRemove`, which is not group-visible, so the
line as written never described the behaviour it appears to promise. After this
task an affected group receives the member-update CMD always, and a group-visible
system message only on a creator handover. That brief is left unedited — it
records what its own PR shipped; this brief is the current statement.

### Existing primitives this task reuses

- **`handOverGroupCreator`** (`modules/group/space_member_removal.go`) — already
  promotes the second-oldest non-bot member in its own transaction, re-reading
  the leaver's role under a row lock. Untouched except for where the notice is
  emitted from.
- **Content type `GroupTransferGrouper` (1008)** — what the manual transfer path
  (`api.go transferGrouper` → `base/event/handler.go` → `SendGroupTransferGrouper`)
  already emits, so the cascade renders through the same client path.
- **`{N}` + `extra` payload shape** — established for multi-name system messages;
  `SendGroupMemberScanJoin` (type 1007) uses `“{0}”通过“{1}”的二维码加入群聊` with
  a two-element `extra`. All three first-party clients substitute generically over
  `extra.length` and none reads 1008 structurally — audited per-client in PR #804
  review, which also flagged that Android's `MessageFormat` treats ASCII `'` as an
  escape, a hazard for any future English translation.

## Behavior contract

### Trigger

Emitted from inside `handOverGroupCreator`, immediately after its transaction
commits, for every reason **except** `space_disbanded`.

Emitting it there rather than from the caller is load-bearing, not stylistic.
The handover commits its own transaction; `RemoveGroupMembers` runs afterwards
and can fail (DB error, invited-bot cascade, the `Removed == 0` concurrency
guard). Announcing after that call loses the message permanently: the job
retries, the leaver is already `MemberRoleCommon`, the handover branch is not
re-entered, and nothing ever announces the owner that did change — reinstating
the exact defect this task exists to remove. At the commit point it is also
idempotent: a retry re-reads the role under the row lock, sees it is no longer
the creator, and returns without re-announcing.

### Payload

| field | value |
|---|---|
| `type` | `common.GroupTransferGrouper` (1008) |
| `content` | `“{0}”已离开当前空间，“{1}”已成为新群主` |
| `extra[0]` | leaver `{uid, name}` |
| `extra[1]` | successor `{uid, name}` |
| `Subscribers` | unset — group-visible |
| `payload.visibles` | unset — group-visible |
| `Header.RedDot` | `1`, matching every group system message in octo-lib |
| `Header.NoPersist` | `0` — stays in group history |

Both names travel in `extra` behind placeholders and are **never concatenated
into `content`**. That is what removes the injection surface: a display name is
user-controlled (`group_member.remark` is settable by the member and by any group
manager, with no content validation), and a name baked into a persisted,
group-visible string could forge extra lines of "system notice".

It does **not** buy "a later rename leaves no stale name": all three first-party
clients render `extra[i].name` directly and none re-resolves by uid, so a
`NoPersist: 0` message keeps the name it was sent with. An earlier revision of
this brief claimed otherwise (PR #804 review). The uid in `extra` makes
re-resolution possible; nothing does it today.

The payload is built in `modules/group` rather than by calling
`SendGroupTransferGrouper`, because that primitive's text is fixed at the
one-clause form and the cascade needs the cause stated. The content type is kept
identical so one event does not render two ways.

### Chain suppression

Before announcing, the elected successor is checked against
`space_member_removal_cleanup` for a `status=pending` row in the same Space
(`spacemod.HasPendingRemovalCleanup`). If one exists, the handover still happens
but the notice is skipped: this link is mid-chain and is about to be superseded.

Only the final link — whose successor is not queued for removal — announces, and
what it announces is the settled owner. The outcome does not depend on the order
the jobs run in: within one batch, "the successor is not pending" holds for
exactly one link whichever order they execute.

`done` and `abandoned` do not suppress. `done` means that job already ran, so the
member is no longer a live candidate anyway; `abandoned` means the removal gave
up, so that member is staying and their handover is real.

### Display names

**This also changes `sendGroupExitTip`**, which previously resolved the global
`user.name` only. Keeping the change (consistency with `groupExit` and the roster
is this repository's rule) rather than scoping it to the new path, and pinning it
with a test — it arrived as a side effect of a parameter refactor and was
unguarded until PR #804 review pointed it out.

Group `remark` first, falling back to global `user.name`, falling back to uid —
the repository's existing rule (`groupExit` resolves `loginMember.Remark` first;
the roster renders `Remark`). The successor's name comes from the row the
handover transaction already holds under `FOR UPDATE`: no extra query, and no
window in which a re-read could observe a different row.

### Suppression

| reason | handover performed | notice sent |
|---|---|---|
| `kicked` | yes | yes |
| `force_removed` | yes | yes |
| `left` | yes | yes |
| `space_disbanded` | yes | **no** |

Disband is suppressed because it removes *every* member, so the handover chains
down the seniority list (C→S2, S2→S3, …). An M-member group would emit M-1
notices, the first M-2 already false when written, into a Space that no longer
exists.

## Load-bearing list

- **`space` / `isolation`** — the notice is emitted on the Space-removal cascade
  path and is group-visible, so it must not carry Space-scoped facts to members
  of other Spaces. The shipped wording names no Space; `“{0}”已离开当前空间` is
  read relative to the group's own Space.
- **`wire-contract`** — adds a message on content type 1008 whose `content` and
  `extra` arity differ from `SendGroupTransferGrouper`'s. Clients must tolerate a
  second placeholder and a two-element `extra` on that type.
- **`external-content` / `escape`** — a user-controlled display name reaches a
  persisted, group-visible message. Structural (`extra` + placeholder), not
  string interpolation.
- **`concurrency`** — the notice is emitted from inside a function that runs
  under a leased worker with retries and possible concurrent re-claim; it must be
  neither lost on retry nor duplicated.
- **`testing`** — the guards here are assertions about *visibility*, which the
  pre-existing tests did not distinguish from "was sent".
- **`commit`** — English Conventional Commits.

## Out of scope

- **A per-member departure broadcast.** Both removal endpoints accept
  `managerMaxBatchUIDs` = 200 uids and enqueue one cleanup job per uid, each
  walking every group the member is in: 200 members across 50 groups is 10,000
  permanent group-visible messages — the same order as the disband case #795
  already suppresses for that reason. The removed member still gets the private
  notice; the roster refreshes via CMD.

  **Correction (PR #804 review).** An earlier revision of this brief claimed the
  handover notice is "at most one per group regardless of batch size" because it
  fires only on a handover. That was false, and measured false: a 3-uid batch
  whose members are consecutive in one group's seniority list produced **three**
  notices in that group (C→S2, S2→S3, S3→S4), the first two already obsolete when
  written. It is the same chaining the disband path is suppressed for; only the
  trigger differs. The invariant is now enforced rather than assumed — see
  Behavior contract › Chain suppression.
- **The ordinary group-kick and bot-API paths.** `modules/group/api.go`
  `memberRemove` and `modules/bot_api/groups.go` reach the same
  `SendGroupMemberBeRemove`, so remaining members see nothing there either. Same
  defect, different entry points, **not tracked in #797** — wants its own task.
- **Coalescing several removals into one message per group.** Would require the
  outbox to carry batch identity; not needed once the notice is handover-only.
- **Everything already deferred to #797** — the IM-unsubscribe leak, the residual
  ABBA and its missing index, the rejoin window.
- **A bot as successor.** `querySecondOldestNonBotMemberTx` excludes only the
  leaver's own active bots, so a third party's bot — or the leaver's own
  *inactive* bot — can be promoted and therefore announced. Pre-existing
  selection behaviour, deliberately pinned by
  `TestQuerySecondOldestMemberExcludingBotsOf_OnlyBotsLeft` ("他人的 bot 不在排除
  范围内（与旧 QuerySecondOldestMember 语义一致）"); this task makes a previously
  silent outcome visible but does not change the selection. `group_member.robot`
  is populated (`service.go:1298`, `:1328`) so `gm.robot = 0` would be a one-line
  fix, but reversing a deliberately pinned contract needs its own decision.

- **A stale pending job suppressing a valid notice.** The chain check treats any
  `status=pending` row for the successor as "they are leaving too". A successor
  carrying an older, stuck pending job therefore loses their announcement. The
  opposite failure — announcing an owner who is about to be removed — is worse,
  so the check is deliberately biased this way. A DB error on the check is
  treated as "not pending" and the notice is sent, for the same reason.

## Acceptance

All in `modules/group/space_member_removal_test.go`, all passing under
`-race -shuffle=on`:

- `TestGroupCascadeCreatorHandoverIsAnnounced` — a force-removed creator produces
  a group-visible message (no `subscribers`, no `visibles`) containing both
  clauses, with `extra[0]` the leaver and `extra[1]` the successor, exactly once.
- `TestGroupCascadeHandoverAnnouncedOncePerRetry` — with `RemoveGroupMembers`
  fault-injected to fail once *after* the handover commits, a retry yields exactly
  one notice: not zero (lost) and not two (duplicated). **The first version of
  this test was vacuous** (PR #804 review): it merely ran the step twice, and the
  second run returned early at `queryGroupsWithMemberUIDAndSpaceID` because the
  member row was already deleted, so the handover path was never re-entered and
  `count == 1` held under either placement. The fault injection is what makes the
  test discriminate.
- `TestGroupCascadeHandoverPrefersGroupRemark` — the successor's group `remark`
  wins over the global `user.name`.
- `TestGroupCascadeLoneCreatorAnnouncesNoHandover` — no successor, no notice.
- `TestGroupCascadeDisbandSuppressesHandoverAnnounce` — disband still performs the
  handover but sends no notice.
- `TestGroupCascadeKickSendsNoGroupBroadcast` /
  `TestGroupCascadeForceRemovedSendsNoGroupBroadcast` — an ordinary member's
  removal produces **zero** group-visible messages carrying visible content
  (excluding the type=99 CMD), while the removed member's own private notice is
  still sent.
- `TestGroupCascadeBatchHandoverAnnouncesOnce` — a 3-uid batch sharing one group
  emits exactly one notice, naming the **final** owner rather than a mid-chain one.
- `TestGroupCascadeSelfExitTipPrefersGroupRemark` — pins the `sendGroupExitTip`
  display-name change this task makes (see below).
- `TestGroupCascadeHandoverNoticeIsBestEffort` — a failing send does not fail the
  job; the handover stays committed and the removal completes.
- Pre-existing suppression guards still hold:
  `TestGroupCascadeSelfExitSuppressesRemovedNotice`,
  `TestGroupCascadeDisbandSuppressesPerMemberNotice`,
  `TestGroupCascadeKickStillSendsBotTip`.

Each guard is mutation-checked individually: reverting it turns its own test red
and only that one.

Gates: `gofmt`, `go vet`, `make i18n-extract-check`, `make i18n-lint`, and
`pkg/space` + `modules/space` + `modules/group` + `modules/bot_api` under
`-race -shuffle=on` with the per-package database reset CI uses.
