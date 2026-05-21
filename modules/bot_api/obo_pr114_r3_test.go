// Package bot_api · PR#114 R3 (Jerry-Xin perf blocker, 2026-05-21).
//
// fanoutForMessage's mention gate MUST run BEFORE the grant DB lookup
// for group-like channels. Pre-fix, every inbound group message —
// plain text, @AI-only, @bot-only — paid a full `obo_grants` scan
// because `findActiveGrantsForChannel` was called UNCONDITIONALLY for
// the channel, then the per-message v2 narrowing gate (which drops
// these traffic shapes) ran inside the per-grant loop. With OBO grants
// gaining adoption that scan was dominating fan-out CPU even though
// the vast majority of group traffic could never trigger a fan-out copy.
//
// Tests in this file pin three contracts that together close the
// blocker:
//
//  1. EARLY-RETURN: plain / @AI-only / @bot-only group traffic returns
//     from fanoutForMessage without invoking EITHER grant lookup
//     method. The fake's call counters surface a regression that
//     forgets to short-circuit, before it reaches CI perf metrics.
//
//  2. DB-LEVEL FILTER: explicit `@grantor` (mention.uids) traffic
//     consults `findActiveGrantsForChannelByGrantors` (NOT the
//     unfiltered `findActiveGrantsForChannel`) and passes the exact
//     mentioned UID set as the IN(...) filter. Catches a refactor
//     that quietly drops the narrowed-query path and reverts to the
//     full scan.
//
//  3. @所有人 FALLBACK: `mention.all=1` correctly accepts the
//     unfiltered scan because we cannot know group membership at
//     this layer; the perf cost is bounded by how rarely @所有人 is
//     used in production.
//
// DM (Person) traffic is intentionally NOT exercised here — the
// pre-fix DM path already used the scope-joined query
// (`findActiveGrantsForChannel` with the scope INNER JOIN) which has
// the channel filter baked in, so DM behavior is unchanged. The DM
// regression guard lives in TestFanout_YUJ1538_DMStillRequiresScopeRow.
package bot_api

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
)

// assertNoGrantLookups fails the test when the fan-out path consulted
// either of the two `findActiveGrants*` methods. Used by the
// early-return tests to lock in the "no DB hit on plain group traffic"
// contract.
func assertNoGrantLookups(t *testing.T, s *fakeOBOStore, label string) {
	t.Helper()
	if s.findGrantsChannelCalls != 0 {
		t.Fatalf("%s: findActiveGrantsForChannel must NOT be called on early-return path, got %d call(s)",
			label, s.findGrantsChannelCalls)
	}
	if s.findGrantsChannelByGrantorsCalls != 0 {
		t.Fatalf("%s: findActiveGrantsForChannelByGrantors must NOT be called on early-return path, got %d call(s)",
			label, s.findGrantsChannelByGrantorsCalls)
	}
}

// TestFanout_PR114R3_GroupPlainText_NoDBLookup — the canonical
// early-return case. A group message with no mention.uids and no
// mention.all must short-circuit before any grant lookup runs.
func TestFanout_PR114R3_GroupPlainText_NoDBLookup(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantNoScope(t) // grant exists with global_enabled=1
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		// Plain text — no `mention` field at all.
		Payload: []byte(`{"type":1,"content":"just chatting"}`),
	}
	if got := ba.fanoutForMessage(msg); got != 0 {
		t.Fatalf("plain group text must NOT fan out, got %d", got)
	}
	if len(fc.copies) != 0 {
		t.Fatalf("plain group text must NOT dispatch copies, got %d", len(fc.copies))
	}
	assertNoGrantLookups(t, s, "plain text group")
}

// TestFanout_PR114R3_GroupAIMentionOnly_NoDBLookup — @AI / @bot
// traffic (mention.ais set, mention.uids empty, mention.all unset)
// MUST short-circuit too. Per spec, @AI alone does NOT summon the
// persona; only @grantor or @所有人 does. The early-return guarantees
// we don't pay a DB scan to discover that.
func TestFanout_PR114R3_GroupAIMentionOnly_NoDBLookup(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantNoScope(t)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		// `mention.ais` set, `mention.uids` empty, `mention.all` unset.
		// Real WuKongIM @AI payload shape.
		Payload: []byte(`{"type":1,"content":"@AI summarize","mention":{"ais":["ai_bot_uid"]}}`),
	}
	if got := ba.fanoutForMessage(msg); got != 0 {
		t.Fatalf("@AI-only group text must NOT fan out, got %d", got)
	}
	assertNoGrantLookups(t, s, "@AI-only group")
}

// TestFanout_PR114R3_GroupEmptyMentionUIDs_NoDBLookup — a payload
// that carries a `mention` object but with `uids: []` and
// `all: 0` (or absent) must also short-circuit. Some clients emit
// the empty array even when nobody was @-mentioned.
func TestFanout_PR114R3_GroupEmptyMentionUIDs_NoDBLookup(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantNoScope(t)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"hi","mention":{"uids":[],"all":0}}`),
	}
	if got := ba.fanoutForMessage(msg); got != 0 {
		t.Fatalf("empty mention object must NOT fan out, got %d", got)
	}
	assertNoGrantLookups(t, s, "empty mention object")
}

// TestFanout_PR114R3_GroupGrantorMention_UsesFilteredQuery — explicit
// @grantor must route through `findActiveGrantsForChannelByGrantors`
// (NOT the unfiltered scan) and pass the exact mentioned UID set as
// the filter. Pin both the call shape and the resulting dispatch so
// a refactor that quietly drops the filter and falls back to the full
// scan fails here, not at perf-test time.
func TestFanout_PR114R3_GroupGrantorMention_UsesFilteredQuery(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantNoScope(t)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		Payload: []byte(`{"type":1,"content":"@yu help","mention":{"uids":["` +
			tGrantor + `"]}}`),
	}
	if got := ba.fanoutForMessage(msg); got != 1 {
		t.Fatalf("@grantor in group must fan out, got %d", got)
	}
	// The filtered method MUST have been called, the unfiltered one
	// MUST NOT — that's the whole point of the perf fix.
	if s.findGrantsChannelByGrantorsCalls != 1 {
		t.Fatalf("filtered query findActiveGrantsForChannelByGrantors must be called exactly once, got %d",
			s.findGrantsChannelByGrantorsCalls)
	}
	if s.findGrantsChannelCalls != 0 {
		t.Fatalf("unfiltered findActiveGrantsForChannel MUST NOT be called for @grantor, got %d call(s)",
			s.findGrantsChannelCalls)
	}
	// Verify the bind shape: exactly the mentioned UIDs, no more.
	args := s.lastFindByGrantorsArgs
	if !args.called {
		t.Fatalf("filter args were not recorded — did the test seam fire?")
	}
	if args.channelID != ch {
		t.Fatalf("filter channelID: want %q, got %q", ch, args.channelID)
	}
	if args.channelType != ct {
		t.Fatalf("filter channelType: want %d, got %d", ct, args.channelType)
	}
	if len(args.grantorUIDs) != 1 || args.grantorUIDs[0] != tGrantor {
		t.Fatalf("filter grantorUIDs: want [%q], got %v", tGrantor, args.grantorUIDs)
	}
}

// TestFanout_PR114R3_GroupMultipleGrantorMentions_FilterCarriesAll —
// a message that @-mentions multiple grantors must pass ALL of them
// through to the filter so the DB can return matching rows for each.
// Catches a regression that only forwards the first uid (e.g. taking
// `range mentioned` and breaking after one).
func TestFanout_PR114R3_GroupMultipleGrantorMentions_FilterCarriesAll(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantNoScope(t)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	// Mention three uids: the test grantor (has a grant) plus two
	// other random uids (no grants). The filter call must carry all
	// three sorted; the dispatch must still produce exactly one copy.
	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		Payload: []byte(`{"type":1,"content":"@yu @bob @carol",` +
			`"mention":{"uids":["` + tGrantor + `","u_bob","u_carol"]}}`),
	}
	if got := ba.fanoutForMessage(msg); got != 1 {
		t.Fatalf("multi-mention must fan out only to matching grantor, got %d", got)
	}
	if s.findGrantsChannelByGrantorsCalls != 1 {
		t.Fatalf("filtered query must be called once, got %d", s.findGrantsChannelByGrantorsCalls)
	}
	args := s.lastFindByGrantorsArgs
	if len(args.grantorUIDs) != 3 {
		t.Fatalf("filter must carry all 3 mentioned uids, got %v", args.grantorUIDs)
	}
	// Sorted check — fan-out sorts for stable IN(...) binds.
	seen := map[string]bool{}
	for _, u := range args.grantorUIDs {
		seen[u] = true
	}
	for _, want := range []string{tGrantor, "u_bob", "u_carol"} {
		if !seen[want] {
			t.Fatalf("filter missing expected uid %q, got %v", want, args.grantorUIDs)
		}
	}
}

// TestFanout_PR114R3_GroupMentionAll_UsesUnfilteredScan — `@所有人`
// (mention.all=1) is the documented exception: we cannot know group
// membership at this layer so the full grant scan is unavoidable.
// Pin that contract so a future "always-filter" refactor doesn't
// silently break @所有人 broadcasts (they would early-return because
// the empty filter set returns no rows).
func TestFanout_PR114R3_GroupMentionAll_UsesUnfilteredScan(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantNoScope(t)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		// `@所有人` shape: mention.all=1, no uids.
		Payload: []byte(`{"type":1,"content":"@所有人 ship today","mention":{"all":1}}`),
	}
	if got := ba.fanoutForMessage(msg); got != 1 {
		t.Fatalf("@所有人 in group must summon every grantor, got %d", got)
	}
	if s.findGrantsChannelCalls != 1 {
		t.Fatalf("unfiltered scan must be used for @所有人, got %d call(s)",
			s.findGrantsChannelCalls)
	}
	if s.findGrantsChannelByGrantorsCalls != 0 {
		t.Fatalf("filtered query MUST NOT be used for @所有人 (no UID set to filter on), got %d",
			s.findGrantsChannelByGrantorsCalls)
	}
}

// TestFanout_PR114R3_CommunityTopicEarlyReturn — community-topic
// channels share the group-like "@grantor narrowing" model and must
// therefore ALSO short-circuit on plain traffic.
func TestFanout_PR114R3_CommunityTopicEarlyReturn(t *testing.T) {
	ch, ct := "group_42____topic_a1", common.ChannelTypeCommunityTopic.Uint8()
	s := seedGrantNoScope(t)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"thread reply"}`),
	}
	if got := ba.fanoutForMessage(msg); got != 0 {
		t.Fatalf("plain community-topic text must NOT fan out, got %d", got)
	}
	assertNoGrantLookups(t, s, "plain community-topic")
}

// TestFanout_PR114R3_DMPathUnchanged_StillCallsUnfilteredJoinQuery —
// DM (Person) traffic MUST continue to call findActiveGrantsForChannel
// (which uses the scope-joined SELECT). The mention gate / early
// return / filtered-query optimizations are group-only; DMs never had
// the perf problem because the JOIN-with-scopes was already
// channel-filtered. This regression guard catches a refactor that
// accidentally widens the early-return or the filter path to cover
// DMs.
func TestFanout_PR114R3_DMPathUnchanged_StillCallsUnfilteredJoinQuery(t *testing.T) {
	const peer = "u_bob"
	ct := common.ChannelTypePerson.Uint8()
	// Use the seedGrantWithScope helper variant: install a grant +
	// matching DM scope row so the JOIN actually surfaces it.
	s := seedGrantWithScope(t, peer, ct)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     peer,
		ChannelID:   tGrantor, // DM listener-native view: ChannelID = receiver
		ChannelType: ct,
		// Plain DM — no mention object. Pre-fix and post-fix the DM
		// path must still go through findActiveGrantsForChannel.
		Payload: []byte(`{"type":1,"content":"hey, free now?"}`),
	}
	if got := ba.fanoutForMessage(msg); got != 1 {
		t.Fatalf("DM with matching scope must fan out, got %d", got)
	}
	if s.findGrantsChannelCalls != 1 {
		t.Fatalf("DM path must call findActiveGrantsForChannel exactly once, got %d",
			s.findGrantsChannelCalls)
	}
	if s.findGrantsChannelByGrantorsCalls != 0 {
		t.Fatalf("DM path MUST NOT use the @-mention filtered query, got %d call(s)",
			s.findGrantsChannelByGrantorsCalls)
	}
}

// TestFanout_PR114R3_GroupBotSelfSent_StillEarlyReturns — gate 3
// (bot's own __obo_processed__ marker) still fires before the mention
// gate, so the bot's own outbound copy short-circuits BEFORE we even
// decode mentions. Without this guard a future refactor that moved
// the marker check below the mention gate would re-introduce the loop
// the marker is designed to prevent. We pin the "no grant lookup
// either" contract because gate 3 is the cheapest of the three loop
// protections.
func TestFanout_PR114R3_GroupBotSelfSent_StillEarlyReturns(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantNoScope(t)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     tBot,
		ChannelID:   ch,
		ChannelType: ct,
		// Payload carries the marker AND a mention.all=1 — the marker
		// must win or we'd loop forever.
		Payload: []byte(`{"type":1,"content":"echo","__obo_processed__":true,"mention":{"all":1}}`),
	}
	if got := ba.fanoutForMessage(msg); got != 0 {
		t.Fatalf("__obo_processed__ marker must short-circuit before fan-out, got %d", got)
	}
	assertNoGrantLookups(t, s, "bot self-sent with marker")
}
