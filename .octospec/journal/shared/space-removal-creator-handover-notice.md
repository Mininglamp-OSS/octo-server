---
type: Journal
title: "Journal: space-removal-creator-handover-notice"
description: A Space removal that hands a group to a new owner now says so, in one group-visible message naming both the departure and the successor. The per-member departure broadcast an earlier revision shipped was removed — it was the source of eight review findings, all of which dissolved with it. Also fixes a real defect in the first attempt, where the announcement was permanently lost whenever the cleanup job retried.
tags: ["space", "isolation", "wire-contract", "external-content", "escape", "concurrency", "testing"]
timestamp: 2026-08-23T11:00:00Z
# --- octospec extension fields ---
task: space-removal-creator-handover-notice
upstream: follow-up to Mininglamp-OSS/octo-server#795
source: self
---

# Journal: space-removal-creator-handover-notice

## What was done

`#795` made Space removal cascade into the Space's groups. The reported symptom
afterwards was that the cleanup happened but nothing was said:

> 相比没有 795 之前会清理群聊了, 但是没有系统消息

The diagnosis was not "no message was sent". A message *was* sent —
`SendGroupMemberBeRemove` — but octo-lib pins **both** of its recipient layers
(top-level `subscribers` and `payload.visibles`) to the removed member, and its
text is second-person. It is by design a private notice to the person being
removed. Measured by recording `/message/send` for a kick cascade in a
three-member group, the two remaining members received exactly one thing: a
`type=99` CMD with no visible content.

Shipped: one group-visible message, emitted only when the removal caused a
creator handover.

> `“{0}”已离开当前空间，“{1}”已成为新群主`

on content type `GroupTransferGrouper` (1008) — the same type the manual transfer
path emits, so one event does not render two ways — with both names in `extra`
behind placeholders rather than concatenated into the string.

An ordinary member's removal stays silent, deliberately. That is the second half
of the design and the more consequential decision; see below.

## What changed between the first attempt and the shipped version

The first revision of this branch broadcast a departure notice for **every**
removed member. Adversarial review produced fourteen findings against it. Eight
of them turned out to be consequences of that one decision rather than
independent defects, and all eight disappeared when it was dropped:

| finding | fate |
|---|---|
| N×M volume (200-uid batch × M groups = 10k permanent messages) | gone as a *per-member* flood; the residual handover chain surfaced in round 2 below |
| `sanitizeTipName` missed U+2028/U+2029/U+202E/U+200B | gone — no interpolation left to sanitize |
| a load-bearing comment claiming generic `Tip` has no `{0}`+`extra` precedent | gone — refuted, and the hand-rolled payload it justified is gone |
| `RedDot: 0` + `NoUpdateConversation` left the "broadcast" with no UI signal | gone — 1008 carries `RedDot: 1` like every sibling |
| name resolved remark-first while the adjacent bot-cascade tip used the global name | gone — no adjacent pair any more |
| "当前空间" disclosed to external members from another Space | gone — wording is group-relative |
| a false claim pinned in history when the member rejoined | gone |
| content type split from the manual transfer path (2000 vs 1008) | gone — same type |

The lesson is in `learnings/pending/eight-findings-one-decision.md`: when review
findings cluster, check whether they share a parent decision before fixing them
one by one.

## The defect the first attempt introduced

Worth recording because it was subtle and six independent review angles found it.

`handOverGroupCreator` commits its own transaction. The first version announced
the handover from the **caller**, after `RemoveGroupMembers`. That call can fail
several ways (DB error, invited-bot cascade, the `Removed == 0` concurrency
guard), and on failure the cleanup job retries. On the retry the leaver is
already `MemberRoleCommon`, so the handover branch is not re-entered, the
successor variable stays empty, and the announcement is **never sent** — exactly
the silent owner change the change existed to remove.

Moving the announcement to the commit point inside `handOverGroupCreator` fixes
it and is idempotent for free: a retry re-reads the role under the row lock, sees
it is no longer the creator, and returns without re-announcing.
`TestGroupCascadeHandoverAnnouncedOncePerRetry` pins this — though not in its
first form, which review showed was vacuous; see round 2.

## Two things I got wrong, recorded so the next reader does not trust them

1. **"Generic `Tip` (2000) has no `{0}` + `extra` precedent, and the substitution
   cannot be confirmed from this repo."** Stated confidently, used to justify
   hand-rolling a payload and writing a sanitizer, and **false**. Three in-repo
   call sites use it, one of them `modules/group/event.go` — the group-disband
   notice, in the same package. The claim came from grepping a single sibling
   (`sendBotCascadeRemovedTip`) and generalising.

2. **`sanitizeTipName` did not do what its own doc comment claimed.** Executed
   against the real function: `unicode.IsControl` covers only category Cc, so
   `\n` was stripped while U+2028, U+2029, U+202E and U+200B all survived — it
   blocked the character the web client would have collapsed anyway and passed
   the ones that actually force a line break. A name of only zero-width
   characters also bypassed the empty-name fallback. Deleted rather than widened.

## Testing

Full stack locally (MySQL 8.0.46 + Redis 7 + WuKongIM v2.2.4-20260313), with the
per-package database reset CI uses.

- `pkg/space`, `modules/space`, `modules/group`, `modules/bot_api` — all pass
  under `-race -shuffle=on`.
- `gofmt`, `go vet`, `make i18n-extract-check`, `make i18n-lint` — clean.
- Every guard mutation-checked individually: reverting one turns its own test red
  and only that one.

Two verification notes worth keeping:

- **A mutation that should have failed, passed.** The "no departure broadcast"
  guard asserted on a content fragment; the mutation re-added a broadcast with
  the name in `extra`, so the fragment never matched and the test stayed green.
  Fixed by counting group-visible messages that carry any visible content
  (excluding the `type=99` CMD) instead of matching text. Recorded as
  `learnings/pending/mutation-check-the-assertion-not-the-guard.md`.
- **An intermittent `modules/group` failure under `-shuffle=on` is not ours.**
  `TestGroupCreate_WithCategoryID` + `TestGroupSettingUpdate_AllowNoMention_*`
  fail roughly 1 run in 10. Merge base `fe9ddeb` reproduces the same pair at the
  same rate. Root cause is the connection-pool exhaustion already documented in
  `ci.yml` — local MySQL defaults to `max_connections=151`; raising it to 1000 as
  CI does makes both the merge base and this branch green 5/5.

## Follow-ups

- **The ordinary group-kick and bot-API paths have the same defect.**
  `modules/group/api.go` `memberRemove` and `modules/bot_api/groups.go` reach the
  same `SendGroupMemberBeRemove`, so remaining members see nothing there either.
  **Not tracked in #797** — checked the whole issue.
- **A third party's bot can be promoted to creator and is now announced.**
  `querySecondOldestNonBotMemberTx` excludes only the leaver's own active bots; a
  bot owned by someone else is an eligible successor, pinned as an existing
  contract by `bot_cascade_test.go`. This change makes a previously silent
  outcome publicly visible without altering the selection.

## Round 2 — what PR #804 review found

Two blocking findings, both **measured by the reviewers rather than argued**, and
both reproduced here before being fixed.

### The batch chain (P1-1)

The brief and PR claimed the notice is "at most one per group regardless of batch
size", reasoning that firing only on a handover bounds it. False. A batch removes
uids independently (one cleanup job each), so when the removed members are
consecutive in a group's seniority list the handover chains and each link
announces. Measured with a 3-uid batch in one group:

```
group-visible 「已成为新群主」 in ONE group: 3   (claimed ≤1)
   extra[0]=u-c  -> extra[1]=u-s2
   extra[0]=u-s2 -> extra[1]=u-s3
   extra[0]=u-s3 -> extra[1]=u-s4
```

The first two were already false when written. This is the same chaining the
disband path is suppressed for — the reasoning transferred verbatim and I did not
notice, despite having written that reasoning into the code comment myself.

Fixed by checking whether the elected successor is itself `pending` in
`space_member_removal_cleanup` before announcing. Mid-chain links stay silent; the
final link announces the settled owner. Order-independent.

### The vacuous guard (P1-2)

`TestGroupCascadeHandoverAnnouncedOncePerRetry` did not test what it claimed.
Running the step twice does not produce a retry: the second run returns early at
`queryGroupsWithMemberUIDAndSpaceID` because the member row is already deleted, so
the handover path is never re-entered and `count == 1` holds trivially under
either placement. Two reviewers independently applied the placement mutation and
watched all five tests stay green.

I had claimed "every guard mutation-checked individually". That was true of the
mutations I ran, and the mutation that mattered was not among them: I had mutated
"disable the notice" (red) but never "move where it is sent from" — which is the
decision the test exists to protect. **A guard is only as good as the mutation you
aim at it, and the mutation to aim is the one that reintroduces the defect, not
the one that deletes the feature.**

Rewritten with `RemoveGroupMembers` fault-injected to fail once after the handover
commits. It now goes red (`expected 1, actual 0`) under the placement mutation.

One detour worth recording: my *first* attempt at that mutation also stayed green,
and that was a flaw in the mutation, not the test — I passed the successor through
a package-level variable that survived across calls, where the real "announce in
the caller" shape uses a local reset per call. A mutation has to be faithful to
the code it is impersonating, or a green result means nothing.

### Also from review

- Three comment blocks contradicted the shipped code — leftovers from the
  abandoned broadcast revision. Removed or rewritten.
- "A later rename does not leave a stale name" was false: all three clients render
  `extra[i].name` and none re-resolves by uid. Corrected in both code and brief.
- `sendGroupExitTip`'s display name silently changed to remark-first as a side
  effect of a parameter refactor — outside the stated scope and unguarded. Kept
  (it matches the repo's rule) and now pinned by a test.
- Nothing pinned the `GroupTransferGrouper` content type, nor the best-effort
  delivery decision. Both now have guards.
- Android's `MessageFormat` treats ASCII `'` as an escape — a future English
  translation would silently lose the placeholders there. Recorded in the code.

A third, automated reviewer approved the same head, having noticed the identical
test weakness but classified it as a comment-accuracy issue. It stated plainly
that it ran no code. The two humans ran the mutation. Same observation, opposite
verdict, and the difference was execution.
