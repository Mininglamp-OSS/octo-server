---
type: Learning
title: "Relationship gates: the friend table is not the deployment-portable signal"
description: A DM/peer access check written as \"is friend?\" silently becomes a blanket 403 in the enterprise contact-book (Space) deployment, where nothing writes friend rows. Any new peer gate must use the friend ∪ own-bot ∪ same-Space predicate, and bot classification must precede the Space branch because bots have no space_member row.
tags: ["space", "isolation", "auth", "acl", "dm", "review"]
timestamp: 2026-07-28T09:40:00Z
# --- octospec extension fields ---
source: self
origin_task: dm-reaction-space-access
origin_pr: "Mininglamp-OSS/octo-server (DM reaction Space access)"
status: pending
candidate_rule: space-isolation
---
# Relationship gates: the friend table is not the deployment-portable signal

`IsFriend(a, b)` reads one row from `friend`. In the personal-space deployment
that table is authoritative. In the enterprise contact-book (**Space**)
deployment **nothing writes it** on Space join — the only writers are the
system-bot / fileHelper / botfather seeds and the explicit add-friend flow — so
a gate written as

```go
isFriend, err := m.userService.IsFriend(loginUID, req.ChannelID)
if !isFriend { /* deny */ }
```

is not "a stricter check". It is a **blanket deny for every colleague pair**,
for every user, in that deployment. The failure is invisible in unit tests
(which seed a friend row), invisible in the personal-space deployment, and
looks like a permission bug rather than a missing-relationship bug in
production — the caller sees a plain 403.

## The portable predicate

Use the same four steps as `modules/user/api_pinned.go::validateChannelAccess`
(also `modules/messages_search/authz.go::checkP2PAccess`), in this order:

1. `peer == caller` — notes-to-self; no relationship judgement is meaningful.
2. `IsFriend(caller, peer)` — authoritative in personal-space deployments;
   cheapest, so it short-circuits the common legacy case.
3. `QueryPeerRobotInfo(peer)` — own bot passes; someone else's bot must be a
   friend (matches `modules/robot/event.go`). A disabled bot yields
   `creatorUID == ""`, so it correctly stays friendship-gated.
4. `AreSpaceMembers(spaceID, caller, peer)` — the Space signal, using only the
   `space_id` the Space middleware already verified.

**Ordering is load-bearing, not stylistic: step 3 must precede step 4.** The two
bot flavours each supply an independent reason, and the security-relevant one is
easy to get backwards:

- robot-module bots **do** carry a `space_member` row — that row is the mechanism
  that makes a bot visible in a Space (`modules/robot/api.go:1497` lists them via
  `space_member INNER JOIN user … u.robot = 1`). A Space-first gate would let
  *someone else's* bot through on Space membership alone, silently dropping the
  friendship requirement that `modules/robot/event.go` enforces.
- App Bots **do not** (`modules/app_bot/app_bot.go createBot` deliberately skips
  it), so a Space-first gate would deny a user their *own* bot's DM.

Do not simplify this to "bots have no `space_member` row" — that half is only
true for App Bots, and a future author who discovers the other half may drop the
ordering constraint as obsolete.

Two more details that keep the gate honest:

- **Fail closed on every lookup.** Return `(false, err)` and let the caller map
  it to the module's internal/query-failed code after logging the cause. A gate
  that treats a DB error as "not a friend" is fine; one that treats it as
  "allow" is a hole.
- **A missing Space on the request is not a reason to invent one.** Resolving
  the caller's default Space as a fallback widens the gate for clients that
  simply omit the header. Degrade to friend-only instead — that keeps the
  predicate identical to the canonical one and costs one query less.

## How to catch it in review

When a diff adds or moves a peer/DM access check, ask: *which deployment mode
does this signal exist in?* Grep for the writers of the table being read
(`friend` here). If the only writers are seeds and an explicit user action, the
gate cannot carry the enterprise deployment on its own.

Origin: `modules/message/api.go` reaction endpoints (write + sync) were the last
two peer gates still on friend-only; the same pattern still exists in
`message/api_pinned.go`, `message/api_channel_files.go`, and `channel/api.go`.
