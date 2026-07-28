---
type: Journal
title: "Journal: dm-reaction-space-access"
description: DM reaction endpoints were gated on the friend table alone, so every colleague DM reaction 403'd in Space deployments; aligned both the write and the sync path with the canonical friend ∪ own-bot ∪ same-Space predicate.
tags: ["space", "isolation", "auth", "acl", "error-response", "testing", "dm", "reaction"]
timestamp: 2026-07-28T09:40:00Z
# --- octospec extension fields ---
task: dm-reaction-space-access
upstream: "production report — POST /v1/reactions on a human-to-human DM returned err.server.message.channel_access_denied (403)"
source: self
---

# Journal: dm-reaction-space-access

## What was done

- Replaced the friend-only DM gate on `POST /v1/reactions`
  (`addOrCancelReaction`) and `POST /v1/reaction/sync` (`syncReaction`) with the
  predicate `modules/user/api_pinned.go::validateChannelAccess` already used:
  self → friend → bot (own bot allowed; someone else's bot still needs
  friendship) → real user in the same active Space.
- Extracted the predicate into `modules/message/api_person_access.go` as a pure
  function over a three-method dependency interface (`IsFriend`,
  `QueryPeerRobotInfo`, `AreSpaceMembers`), which `user.IService` satisfies
  structurally. Read and write call the same helper, so the read/write parity
  invariant already documented in `syncReaction` cannot drift.
- No new error code and no wire change: denials stay
  `ErrMessageChannelAccessDenied`, lookup failures stay
  `ErrMessageQueryFailed` (logged with `zap.Error` first, fail-closed).

## Why it was broken

The Person branch of `addOrCancelReaction` has exactly one path to
`channel_access_denied` — the `!isFriend` check — so the production 403 was
unambiguous. `IsFriend` is a plain `friend` table lookup, and **nothing writes
`friend` rows on Space join**: the only writers are the system-bot / fileHelper /
botfather seeds and the explicit add-friend flow. In the enterprise
contact-book (Space) deployment the table is therefore near-empty, so *every*
colleague DM reaction failed, for every user, on both the write and the read
side — the reaction UI could not even load its state.

The rest of the codebase had already moved to Space membership as the DM
reachability signal and said so in comments
(`api_pinned.go::validateChannelAccess`, `messages_search/authz.go::checkP2PAccess`
"in Space mode the friend table is near-empty", `sendMsg`'s Space branch using
`CheckBothMembers`). The reaction endpoints were the outlier.

## Load-bearing decisions

- **No default-Space fallback.** The Space used is only the
  middleware-verified `space_id`; a request without `space_id` / `X-Space-ID`
  degrades to friend-only (unchanged behavior, and one less query on that path).
  Keeps the predicate byte-identical to `validateChannelAccess`.
- **Bot classification runs before the Space branch**, for two independent
  reasons — the security-relevant one is the reverse of the intuitive one:
  robot-module bots **do** have a `space_member` row (that row is what makes a
  bot visible in a Space, `modules/robot/api.go:1497`), so a Space-first gate
  would admit *someone else's* bot on Space membership alone and drop the
  friendship requirement `modules/robot/event.go` enforces; App Bots **do not**
  have one (`app_bot.go createBot` skips it), so a Space-first gate would deny a
  user their *own* bot's DM. Disabled bots (`user.robot=1`, `robot.status!=1`)
  surface `creatorUID=""` and therefore stay friendship-gated.

- **`syncReaction` rejects composite (`a@b`) `channel_id` at the boundary, and
  the storage channel id is now always derived from `loginUID`.** This is the
  security fix that came out of review (see below); it turns "the physical
  channel id always contains the caller" from an assumption about uid charsets
  into a structural invariant.
- **Prefixed DM channel ids (`s{spaceID}_{uid}`) deliberately not handled.**
  Investigated: the current system does not produce them for DMs — this repo's
  own record states conversation-level `SpaceID` for a DM is intentionally
  empty (`dm-space-isolation-484`, `api_conversation.go:1584`), which is why the
  `payload.space_id` soft tag exists at all. The only non-test constructor
  (`webhook/common.go resolvePushChannelID`) merely preserves a prefix that
  arrived from WuKongIM; everything else is read-side legacy tolerance.
- **Scope held to the two reaction endpoints** even though the identical
  friend-only gate exists on three other endpoints (see Follow-ups).

## Verification

- Red-before-green proved on real infra: with the pre-fix `api.go`,
  `TestReactionAllowsSameSpaceDMWithoutFriendship` fails and
  `TestReactionRejectsDMWithoutFriendshipOrSpace` passes — i.e. the new test
  pins the production bug and the negative test shows the gate was not merely
  opened.
- `-tags integration -run Reaction`: 20/20 pass. Default build (the lane CI
  actually runs) `-race -shuffle=on ./modules/message/`: 380 pass, 0 fail.
- 13-case unit decision table + 3 query-count assertions, all DB-free.
- `go build ./...`, `go vet`, `golangci-lint` (0 issues),
  `make i18n-extract-check`, `make i18n-lint` green.

## The defect review caught (and the general lesson)

The first version of this change made a genuinely exploitable path reachable.
`syncReaction` historically accepted an already-composed physical channel id
(`strings.Contains(req.ChannelID, "@")`) and passed it to storage verbatim — the
only path where the storage channel id was not derived from `loginUID`. The
friend gate had kept that branch dead by accident: every `friend` insert path
resolves the peer through a `user` existence check first, so `IsFriend(u, "a@b")`
could never be true. The Space gate has no such discipline —
`AreSpaceMembers` → `CheckBothMembers` joins `space` and `space_member` but
**never `user`**, and `space_member.uid` is an attacker-writable arbitrary
string (`space/api.go addMembers` inserts `req.UIDs` verbatim with no existence
or charset check, and creating a Space is open to any authenticated user by
default). So: create a Space, add the string `"<victimA>@<victimB>"` as a
"member", then `POST /v1/reaction/sync` with that as `channel_id` — the gate
passes and the victim DM's reaction metadata (message_id, reactor uid + display
name, emoji, seq, timestamps) comes back.

Confirmed empirically, not just by reading: `TestSyncReactionRejectsCompositeChannelIDLeak`
returned `200` with the victim's row before the fix and is denied after.

**Lesson:** when you swap the predicate behind a gate, the question is not "what
values does the *identity* table allow" but "what does each *newly consulted*
table allow, and who can write it". A predicate whose backing table has no
referential discipline can revive input shapes that a stricter predicate had
been keeping dead.

Two follow-ups this exposes, both worth their own task and both wider than this
endpoint: `space/api.go addMembers` should validate that each uid resolves to a
real user, and `CheckBothMembers` should join `user` so "is a Space member"
implies "is a user" for every caller at once.

## Gotcha worth remembering (test infrastructure)

`modules/message` integration tests do **not** run in CI, and there are two
independent walls:

1. `ci.yml` deliberately runs the **default** build with no `-tags integration`,
   because `modules/conversation_ext/*` integration tests connect to a separate
   `conv_ext_test` database with `Migration=false` and assume a pre-baked
   schema; the CI job only provisions `test`. The comment there explicitly flags
   flipping the flag as a maintainer decision.
2. Even with the flag flipped, this package's integration build does not
   compile: `api_card_action_test.go` imports `modules/app_bot` →
   `modules/bot_api` → `modules/messages_search` → `modules/message`, an import
   cycle in test. Reproduced on clean `origin/main`.

Running the suite locally required temporarily setting
`api_card_action_test.go` + `api_card_revisions_test.go` aside (the latter
depends on a symbol in the former).

## Follow-ups / notes

- **Blacklist is not consulted on the DM reaction path.** `addBlacklist`
  (`user/api.go:2396`) writes `user_setting.blacklist=1` and bumps the friend
  version but does not delete the `friend` row, so a blacklisted *friend* could
  already react before this change; the gap now extends to blacklisted
  same-Space colleagues. `messages_search/authz.go` is the only DM gate that
  checks the bidirectional blacklist; `validateChannelAccess` does not either.
  Whether to tighten all three is a product call.
- **Same friend-only gate remains on**: `message/api_pinned.go:59,357,551`
  (pin/unpin/clear, `ErrMessageConversationForbidden`),
  `message/api_channel_files.go:197` (`ErrMessageNotFriend`), and
  `channel/api.go:99` (channel setting, still on legacy `c.ResponseError` so it
  also needs an i18n migration). All three will 403/deny colleagues in Space
  deployments exactly as reactions did.
- **Dead branch**: `syncReaction` accepts a pre-built fake channel id
  (`strings.Contains(req.ChannelID, "@")`, `api.go:1799`), but no such string
  can pass the DM gate (before or after this change), so the branch is
  unreachable and the write path never supported that shape. Worth cleaning up
  separately.
- octo-web: no change needed. Its reaction call passes
  `requestChannel.channelID` unstripped
  (`features/messageReaction/controller.ts:179`), which is inert with bare-UID
  DM channels.
