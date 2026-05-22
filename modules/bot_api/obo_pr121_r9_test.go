// Package bot_api · PR#121 R9 regression guards (Jerry-Xin + lml2468,
// 2026-05-22 blocking — YUJ-1676).
//
// Pre-R9, the implicit-scope feeder at obo_fanout.go:280 was gated to
// `ChannelTypeGroup` only, and findGlobalGrantsWithoutScope at obo_db.go
// also short-circuited for non-Group types. But the rest of PR#121
// supported CommunityTopic:
//
//   - checkOBO allowed implicit scope for group-like channels
//     (obo_check.go:105 / isGroupLikeChannelType).
//   - The send-permission bypass covered CommunityTopic (PR#121 R7).
//   - Gate 4 handled topics (PR#121 R8).
//
// The asymmetry: a topic `mention.all` message with
// `global_enabled=1`, no explicit scope row, grantor in parent group,
// bot NOT in parent group → ZERO fan-out copies. But if the bot
// somehow received the message, checkOBO would authorize the reply.
//
// R9 extends the implicit-scope feeder (in both the prod SQL and the
// in-memory fake) to also serve CommunityTopic. The membership join
// is rooted at the PARENT group; the scope anti-join keeps using the
// topic's own (channel_id, channel_type) so per-topic disables are
// still honoured.
//
// Also pinned here: Gate 4 fail-closed on userIsGroupMember errors.
// Pre-R9 a transient error fell through to dispatch, which produced a
// duplicate fan-out whenever the bot WAS in the (parent) group but
// the membership query errored.
package bot_api

import (
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
)

// TestFanout_PR121R9_CommunityTopic_MentionAll_ImplicitScope_FansOut
// — the headline R9 repro. CommunityTopic + mention.all=1 +
// global_enabled=1 + NO explicit scope row + grantor in parent group
// + bot NOT in parent group → fan-out copy IS dispatched.
//
// Pre-R9 this returned 0 because:
//
//   - The mention.all branch in fanoutForMessage uses
//     findActiveGrantsForChannel, which INNER JOINs obo_scopes and
//     returns 0 rows when no scope exists.
//   - findGlobalGrantsWithoutScope (the implicit-scope feeder) was
//     gated to ChannelTypeGroup only and returned [] for topics.
//
// R9 wires CommunityTopic through the feeder using the parent group
// for membership; the anti-join still uses the topic's own channel id
// so per-topic admin disables are respected.
func TestFanout_PR121R9_CommunityTopic_MentionAll_ImplicitScope_FansOut(t *testing.T) {
	const parentGroup = "group_topic_parent_pr121r9"
	const topicChan = parentGroup + "____topic_pr121r9"
	ct := common.ChannelTypeCommunityTopic.Uint8()

	// Active+global_enabled grant, NO explicit scope row for the topic
	// (matches operator reality: scopes are only ever installed for DMs).
	s := seedGrantNoScope(t)
	// Grantor is in the parent group → satisfies the implicit-scope
	// grantor-membership predicate (the bot must inherit the grantor's
	// read access). Bot is intentionally NOT seeded → satisfies Gate 4
	// (bot not in parent group, so the OBO copy is the sole delivery
	// path to the bot).
	s.seedGroupMember(parentGroup, tGrantor)

	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)
	// If the access override fires for an implicit-scope grant, the
	// SQL pre-validation got bypassed — mirrors the Group regression
	// guard in TestFanout_ImplicitScope_GrantorMember_BotNotMember.
	// The R9 feeder pre-filters on grantor membership at the SQL
	// layer, so the per-grant Go re-check must be skipped (the grant
	// is flagged via implicitGrantIDs).
	ba.oboChannelAccessOverride = func(uid, channelID string, channelType uint8) (bool, error) {
		t.Errorf("implicit-scope topic grant must NOT trigger grantorCanReadChannel (uid=%q chan=%q)", uid, channelID)
		return true, nil
	}

	msg := &config.MessageResp{
		FromUID:     "u_alice", // not bot, not grantor
		ChannelID:   topicChan,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"@所有人 heads up","mention":{"all":1}}`),
	}
	if n := ba.fanoutForMessage(msg); n != 1 {
		t.Fatalf("PR#121 R9: CommunityTopic + mention.all + implicit-scope must fan out, got %d", n)
	}
	if len(fc.copies) != 1 {
		t.Fatalf("PR#121 R9: expected exactly 1 captured copy, got %d", len(fc.copies))
	}
	cp := fc.copies[0]
	if cp.FromUID != tGrantor {
		t.Fatalf("PR#121 R9: fan-out copy FromUID must be grantor %q, got %q", tGrantor, cp.FromUID)
	}
	if cp.ChannelID != tBot {
		t.Fatalf("PR#121 R9: fan-out copy ChannelID must be bot mailbox %q, got %q", tBot, cp.ChannelID)
	}
}

// TestFanout_PR121R9_CommunityTopic_MentionAll_BotInParentGroup_NoFanout
// — companion to the headline test. Same setup, EXCEPT the bot is
// ALSO a parent-group member. The implicit-scope feeder's
// `gm_bot.uid IS NULL` anti-join (Gate 4 baked into the SQL) must
// suppress the grant entirely → zero fan-out copies, because the bot
// already receives the topic message directly via the parent-group
// subscriber pipeline.
//
// Without R9 this case wasn't a regression risk (the feeder never
// fired for topics) but it MUST be safe after the extension —
// otherwise R9 reintroduces the duplicate-fan-out bug Gate 4 was
// created to prevent.
func TestFanout_PR121R9_CommunityTopic_MentionAll_BotInParentGroup_NoFanout(t *testing.T) {
	const parentGroup = "group_topic_parent_pr121r9_gate4"
	const topicChan = parentGroup + "____topic_pr121r9_gate4"
	ct := common.ChannelTypeCommunityTopic.Uint8()

	s := seedGrantNoScope(t)
	// Both grantor and bot are in the parent group.
	s.seedGroupMember(parentGroup, tGrantor)
	s.seedGroupMember(parentGroup, tBot)

	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     "u_alice",
		ChannelID:   topicChan,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"@所有人 heads up","mention":{"all":1}}`),
	}
	if n := ba.fanoutForMessage(msg); n != 0 {
		t.Fatalf("PR#121 R9: bot in parent group → implicit-scope SQL Gate 4 must suppress fan-out, got %d", n)
	}
	if len(fc.copies) != 0 {
		t.Fatalf("PR#121 R9: leaked %d copies, expected 0 (Gate 4 should bite)", len(fc.copies))
	}
}

// TestFanout_PR121R9_CommunityTopic_MentionAll_ExplicitDisableWins —
// the "explicit scope row takes precedence" invariant from PR#121 R5
// / B1 must keep working on the topic path. A topic with an
// explicitly DISABLED scope row (enabled=0) must NOT trigger fan-out
// even when the grant is global_enabled=1 and the grantor is in the
// parent group.
//
// The scope anti-join in findGlobalGrantsWithoutScope uses the
// TOPIC's own (channel_id, channel_type) — never the parent group's
// — so an admin disabling a single topic must not bleed into the
// parent group's other topics. (The parent group itself has its own
// scope row and its own implicit-scope candidacy, both unaffected by
// the topic's disable.)
func TestFanout_PR121R9_CommunityTopic_MentionAll_ExplicitDisableWins(t *testing.T) {
	const parentGroup = "group_topic_parent_pr121r9_disable"
	const topicChan = parentGroup + "____topic_pr121r9_disable"
	ct := common.ChannelTypeCommunityTopic.Uint8()

	s := seedGrantNoScope(t)
	s.seedGroupMember(parentGroup, tGrantor)
	// Bot is intentionally NOT a parent-group member: in the absence
	// of the disabled scope row, this would be a happy-path R9
	// implicit-scope fan-out. The disabled scope row is the only
	// reason fan-out must NOT fire.
	gid, _ := s.findActiveGrantByGrantorBot(tGrantor, tBot)
	if gid == nil {
		t.Fatalf("seedGrantNoScope did not produce a grant for (%s, %s)", tGrantor, tBot)
	}
	if _, err := s.insertScope(gid.ID, topicChan, ct, 0); err != nil {
		t.Fatalf("insertScope (disabled): %v", err)
	}

	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     "u_alice",
		ChannelID:   topicChan,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"@所有人 hi","mention":{"all":1}}`),
	}
	if n := ba.fanoutForMessage(msg); n != 0 {
		t.Fatalf("PR#121 R9: explicit enabled=0 scope on topic must suppress implicit-scope fan-out, got %d", n)
	}
	if len(fc.copies) != 0 {
		t.Fatalf("PR#121 R9: explicit-disable leaked %d copies, expected 0", len(fc.copies))
	}
}

// TestFanout_PR121R9_CommunityTopic_MalformedThreadID_NoFanout —
// defensive: a CommunityTopic message whose channel id is missing
// the parent-group prefix (no `____` separator) must NOT trigger
// implicit-scope fan-out. The feeder fail-closes by leaving
// `membershipGroupID` empty, which the store rejects with an empty
// slice. Mirrors grantorCanReadChannel's fail-closed treatment of
// the same malformed shape.
func TestFanout_PR121R9_CommunityTopic_MalformedThreadID_NoFanout(t *testing.T) {
	const malformed = "no_separator_here"
	ct := common.ChannelTypeCommunityTopic.Uint8()

	s := seedGrantNoScope(t)
	// Seed grantor into a plausible parent group — the test asserts
	// the feeder never gets there because the topic id can't be split.
	s.seedGroupMember("group_unrelated", tGrantor)

	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     "u_alice",
		ChannelID:   malformed,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"@所有人 hi","mention":{"all":1}}`),
	}
	if n := ba.fanoutForMessage(msg); n != 0 {
		t.Fatalf("PR#121 R9: malformed thread id must fail-closed, got %d", n)
	}
}

// TestFanout_PR121R9_Gate4_UserIsGroupMemberError_FailsClosed —
// pre-R9, Gate 4 ignored `userIsGroupMember` errors (`if gErr == nil
// && isBotInGroup`). When the membership query transiently errored
// AND the bot was actually a member, the dispatch loop fell through
// and produced a fan-out copy — bot then received the message twice
// (once via direct WuKongIM delivery as a group member, once via the
// OBO copy). R9 fails closed: log the error and skip the grant.
//
// This test installs an explicit-scope grant (so the implicit-scope
// SQL pre-filter doesn't bypass Gate 4) and forces the membership
// override to return an error. The dispatch must produce ZERO copies.
func TestFanout_PR121R9_Gate4_UserIsGroupMemberError_FailsClosed(t *testing.T) {
	const groupNo = "group_pr121r9_gate4_err"
	ct := common.ChannelTypeGroup.Uint8()

	s := seedGrantWithScope(t, groupNo, ct)

	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)
	// Force a membership error every time Gate 4 asks. The pre-R9
	// behavior would dispatch the fan-out (gErr != nil → `gErr == nil
	// && isBotInGroup` is false → falls through). R9 must catch the
	// error and `continue`.
	ba.oboGroupMemberOverride = func(uid, groupNo string) (bool, error) {
		return false, errors.New("transient DB error from Gate 4 test")
	}

	msg := &config.MessageResp{
		FromUID:     "u_alice",
		ChannelID:   groupNo,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"@yu","mention":{"uids":["` + tGrantor + `"]}}`),
	}
	if n := ba.fanoutForMessage(msg); n != 0 {
		t.Fatalf("PR#121 R9: Gate 4 must fail-closed on userIsGroupMember error, got %d (would risk duplicate fan-out)", n)
	}
	if len(fc.copies) != 0 {
		t.Fatalf("PR#121 R9: Gate 4 fail-closed leaked %d copies, expected 0", len(fc.copies))
	}
}
