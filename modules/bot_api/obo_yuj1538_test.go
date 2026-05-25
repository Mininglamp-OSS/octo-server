// Package bot_api · YUJ-1538 — fan-out trigger must honor global_enabled
// for GROUP / COMMUNITY_TOPIC channels even when no obo_scopes row
// exists for the channel, and must treat `mention.all=1` (`@所有人`) as
// a summon for every grantor in the group.
//
// The pre-fix bug:
//
//   - `findActiveGrantsForChannel` issued an INNER JOIN against
//     obo_scopes, so groups (for which operators never installed
//     channel_type=2 scope rows) produced zero matches and the fan-out
//     copy was never dispatched. PR#109 fixed the symmetric problem in
//     `checkOBO` (the reply-time permission check) for groups but left
//     the fan-out trigger query stuck on the strict JOIN.
//
//   - `decodeMentionUIDs` only looked at `mention.uids`, so `@所有人`
//     traffic (which sets `mention.all=1` but commonly does not
//     re-list every group member in `mention.uids`) silently never
//     triggered fan-out either.
//
// These tests pin the corrected behavior at the unit level — they
// stand up only the in-memory fake store + the fanoutForMessage entry
// point, so a regression that re-tightens the trigger query or the
// narrowing gate fails fast at unit-test time instead of slipping into
// E2E (where the bug was first observed in im-test prod).
package bot_api

import (
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
)

// seedGrantNoScope is the YUJ-1538 setup parity to seedGrantWithScope:
// install an `active=1 AND global_enabled=1` grant for (tGrantor, tBot)
// but do NOT install any `obo_scopes` row. Mirrors the real-world
// production state: operators only ever created channel_type=1 scopes,
// so for groups the grant is on file with no matching scope row.
func seedGrantNoScope(t *testing.T) *fakeOBOStore {
	t.Helper()
	s := newFakeOBOStore()
	gid, err := s.insertGrant(tGrantor, tBot, "auto", "")
	if err != nil {
		t.Fatalf("insertGrant: %v", err)
	}
	enable := 1
	if err := s.updateGrant(gid, "", &enable, nil); err != nil {
		t.Fatalf("updateGrant: %v", err)
	}
	return s
}

// TestFanout_YUJ1538_GroupNoScopeRow_GlobalEnabledFansOut — the core
// bug repro. A grant with `global_enabled=1` but no `obo_scopes` row
// for the group must still trigger fan-out when the grantor is
// @-mentioned in the group. Pre-fix this returned 0 dispatches because
// `findActiveGrantsForChannel`'s INNER JOIN returned zero matches.
func TestFanout_YUJ1538_GroupNoScopeRow_GlobalEnabledFansOut(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantNoScope(t)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"@yu can you help?","mention":{"uids":["` + tGrantor + `"]}}`),
	}
	got := ba.fanoutForMessage(msg)
	if got != 1 {
		t.Fatalf("YUJ-1538: group @grantor with global_enabled grant must fan out without a scope row, got %d", got)
	}
	if len(fc.copies) != 1 {
		t.Fatalf("expected 1 captured copy, got %d", len(fc.copies))
	}
	cp := fc.copies[0]
	if cp.FromUID != tGrantor {
		t.Fatalf("fan-out copy FromUID: want grantor %q, got %q", tGrantor, cp.FromUID)
	}
	if cp.ChannelID != tBot {
		t.Fatalf("fan-out copy ChannelID: want grantee bot %q (its own mailbox), got %q", tBot, cp.ChannelID)
	}
}

// TestFanout_YUJ1538_CommunityTopicNoScopeRow_GlobalEnabledFansOut —
// `community-topic` channels share the same "@grantor narrowing"
// model as groups and must therefore also bypass the scope-row
// requirement when the grant is `global_enabled=1`.
func TestFanout_YUJ1538_CommunityTopicNoScopeRow_GlobalEnabledFansOut(t *testing.T) {
	ch, ct := "group_42____topic_a1", common.ChannelTypeCommunityTopic.Uint8()
	s := seedGrantNoScope(t)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"@yu thoughts?","mention":{"uids":["` + tGrantor + `"]}}`),
	}
	if got := ba.fanoutForMessage(msg); got != 1 {
		t.Fatalf("YUJ-1538: community-topic @grantor with global_enabled grant must fan out without a scope row, got %d", got)
	}
}

// TestFanout_YUJ1538_GroupMentionAllDoesNotSummonPersona —
// Mininglamp-OSS/octo-server#142 / #143 follow-up. Legacy
// `@所有人` (`mention.all=1`) MUST NOT trigger OBO bot / persona
// fan-out. Pre-#143 the `pkg/mentionrewrite` chokepoint silently
// rewrote `all=1` to `ais=1` so legacy traffic auto-fanned-out to
// every persona clone; #142 reverted that rewrite, and this gate
// (`decodeMentionGate` + the `fanoutForMessage` branch) must enforce
// the same contract one layer deeper. With the rewrite gone, a bare
// `mention.all=1` is treated as plain traffic — the persona summon
// path requires an EXPLICIT `mention.ais=1` (`@所有 AI`) or
// `mention.humans=1` (`@所有人` Plan X shape) signal.
//
// Pre-#143 follow-up this test asserted the OPPOSITE behavior
// (`mention.all=1` SHOULD summon every persona). That was the bug:
// the rewrite chokepoint above the gate was load-bearing, and once
// removed, this gate continued the implicit inference. The new
// expectation pins the corrected behavior: zero dispatches for bare
// `mention.all=1`.
func TestFanout_YUJ1538_GroupMentionAllDoesNotSummonPersona(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantNoScope(t)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	// `@所有人 hi` legacy WuKongIM shape: mention.all=1, no uids array,
	// no ais, no humans. The post-#143 contract: NO fan-out.
	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"@所有人 hi","mention":{"all":1}}`),
	}
	if got := ba.fanoutForMessage(msg); got != 0 {
		t.Fatalf("#142/#143: bare mention.all=1 must NOT summon any persona (rewrite chokepoint is gone — gate must not re-create the inference), got %d", got)
	}
	if len(fc.copies) != 0 {
		t.Fatalf("expected 0 captured copies, got %d", len(fc.copies))
	}
}

// TestFanout_YUJ1538_GroupMentionAisSummonsPersona — positive
// companion to TestFanout_YUJ1538_GroupMentionAllDoesNotSummonPersona.
// The post-#143 trigger for "summon every persona in the channel" is
// `mention.ais=1` (Plan X / YUJ-1389 `@所有 AI` broadcast). Pins the
// happy path so a regression that drops the ais branch from
// `decodeMentionGate` / `fanoutForMessage` fails immediately rather
// than silently reverting to the pre-#143 implicit-inference behavior.
func TestFanout_YUJ1538_GroupMentionAisSummonsPersona(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantNoScope(t)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	// `@所有 AI` Plan X shape: mention.ais=1, no uids, no all, no humans.
	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"@所有 AI hi","mention":{"ais":1}}`),
	}
	if got := ba.fanoutForMessage(msg); got != 1 {
		t.Fatalf("YUJ-1538 + #143: @所有 AI (mention.ais=1) in group must summon every grantor's persona, got %d", got)
	}
	if len(fc.copies) != 1 {
		t.Fatalf("expected 1 captured copy, got %d", len(fc.copies))
	}
}

// TestFanout_YUJ1538_GroupMentionAllBooleanShapeDoesNotSummon —
// the boolean shape (`mention.all=true`) of the legacy WuKongIM
// `@所有人` payload must follow the same post-#143 contract: NO
// fan-out. Mirrors TestFanout_YUJ1538_GroupMentionAllDoesNotSummonPersona
// but pins the alternative wire shape. Pre-#143 this asserted the
// opposite (boolean true must summon every persona) and has been
// reversed alongside the rewrite removal.
func TestFanout_YUJ1538_GroupMentionAllBooleanShapeDoesNotSummon(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantNoScope(t)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"@所有人 hi","mention":{"all":true}}`),
	}
	if got := ba.fanoutForMessage(msg); got != 0 {
		t.Fatalf("#142/#143: mention.all=true (boolean) must NOT summon any persona either, got %d", got)
	}
}

// TestFanout_YUJ1538_GroupMentionAisBooleanShapeSummonsPersona —
// boolean-shape positive companion. Some clients emit
// `mention.ais=true` instead of the numeric `1`. The gate must honor
// both shapes for the ais summon path (parity with the legacy
// boolean-shape coverage on `mention.all`).
func TestFanout_YUJ1538_GroupMentionAisBooleanShapeSummonsPersona(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantNoScope(t)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"@所有 AI hi","mention":{"ais":true}}`),
	}
	if got := ba.fanoutForMessage(msg); got != 1 {
		t.Fatalf("YUJ-1538 + #143: mention.ais=true (boolean) must be treated as truthy summon, got %d", got)
	}
}

// TestFanout_YUJ1538_DMStillRequiresScopeRow — Mininglamp-OSS/octo-server#161
// (YUJ-1977) INVERSION. The YUJ-1538 invariant ("DMs remain strict — a
// grant without a matching scope row gets zero dispatches even when
// global_enabled=1") was the bug behind issue #161: a grantor who flips
// `global_enabled=1` on a DM-shaped grant without ever installing a per-
// peer scope row expected fan-out symmetrical to the group-shaped
// `findGlobalGrantsWithoutScope` path (PR#121) and got silence instead.
//
// Post-#161 the DM fan-out path consults `findGlobalGrantsForDM` after
// `findActiveGrantsForChannel`, so a `global_enabled=1` grant with zero
// scope rows now DOES fan out for any DM peer the grantor has live
// access to (the friend-gate / `grantorCanReadChannel` re-check inside
// `fanoutForMessage` enforces the access invariant; the explicit scope
// row is no longer the sole opt-in signal).
//
// This test pins the new behavior — what was previously a "must NOT
// dispatch" assertion is now a "must dispatch exactly one copy"
// assertion. The original YUJ-1538 intent (group fan-out without scope
// rows) is unchanged and still covered by the sibling tests in this
// file; only the DM half of the contract flipped.
//
// The lower-level new tests live in obo_fanout_test.go:
// TestFanout_DM_GlobalEnabled_NoScopes / WithExplicitScope /
// GlobalDisabled_NoScopes — they assert the dispatch payload + dedup
// contract; this test is the YUJ-1538 file's parity assertion that the
// DM-side regression is closed.
func TestFanout_YUJ1538_DMStillRequiresScopeRow(t *testing.T) {
	const peer = "u_bob"
	ct := common.ChannelTypePerson.Uint8()
	s := seedGrantNoScope(t) // grant exists with global_enabled=1, no DM scope row
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     peer,
		ChannelID:   tGrantor, // DM listener-native view: ChannelID = receiver = grantor
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"hey, can we chat?"}`),
	}
	if got := ba.fanoutForMessage(msg); got != 1 {
		t.Fatalf("issue #161 (YUJ-1977): DM with global_enabled=1 + no scope row must fan out via implicit-scope feeder, got %d", got)
	}
}

// TestFanout_YUJ1538_GroupGrantorChannelAccessDenied — TOCTOU
// safeguard. Even with `global_enabled=1` and an explicit @grantor
// mention, fan-out must skip when the grantor has lost access to the
// group (kicked / left). The per-grant `grantorCanReadChannel`
// re-check is the only gate enforcing this once the scope-row layer
// is bypassed for groups, so a regression that drops the check would
// silently leak group traffic into the persona.
func TestFanout_YUJ1538_GroupGrantorChannelAccessDenied(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantNoScope(t)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)
	// Override the live-access re-check to deny — simulates the grantor
	// having been kicked from the group between scope-create and the
	// inbound message.
	ba.oboChannelAccessOverride = func(uid, channelID string, channelType uint8) (bool, error) {
		return false, nil
	}

	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"@yu help","mention":{"uids":["` + tGrantor + `"]}}`),
	}
	if got := ba.fanoutForMessage(msg); got != 0 {
		t.Fatalf("YUJ-1538: grantor without live channel access must NOT receive fan-out copy, got %d", got)
	}
}

// TestFanout_YUJ1538_GroupNoGrantsRegistered_StillSkips — sanity
// check that the widened lookup does not accidentally fan out when
// NO active+global_enabled grants exist system-wide. The cache layer
// short-circuits this path; without that the listener would issue a
// DB lookup per inbound group message even for traffic in groups
// nobody has installed a persona for.
func TestFanout_YUJ1538_GroupNoGrantsRegistered_StillSkips(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := newFakeOBOStore() // no grants at all
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"@yu","mention":{"uids":["` + tGrantor + `"]}}`),
	}
	if got := ba.fanoutForMessage(msg); got != 0 {
		t.Fatalf("YUJ-1538: no grants registered → no fan-out, got %d", got)
	}
}

// TestFindActiveGrantsForChannel_YUJ1538_GroupReturnsGlobalEnabled —
// store-level pin: with no scope rows installed,
// `findActiveGrantsForChannel(group, Group)` returns the grant on
// the GROUP channel type but returns empty on the DM channel type.
// Locks the channel-type asymmetry so a refactor that re-collapses
// the two branches surfaces here, not at fan-out time.
func TestFindActiveGrantsForChannel_YUJ1538_GroupReturnsGlobalEnabled(t *testing.T) {
	s := seedGrantNoScope(t)
	grants, err := s.findActiveGrantsForChannel("group_42", common.ChannelTypeGroup.Uint8())
	if err != nil {
		t.Fatalf("findActiveGrantsForChannel group: %v", err)
	}
	if len(grants) != 1 || grants[0].GrantorUID != tGrantor {
		t.Fatalf("group lookup must return the global_enabled grant, got %+v", grants)
	}
	dmGrants, err := s.findActiveGrantsForChannel("u_bob", common.ChannelTypePerson.Uint8())
	if err != nil {
		t.Fatalf("findActiveGrantsForChannel DM: %v", err)
	}
	if len(dmGrants) != 0 {
		t.Fatalf("DM lookup must STILL require scope row, got %+v", dmGrants)
	}
}

// TestFindActiveGrantsForChannel_PR121R6_CommunityTopicRequiresScopeRow —
// store-level pin for the CommunityTopic branch after PR#121 R6 / B3
// (Jerry-Xin + lml2468 2026-05-22 blocking). The R5 fake treated
// CommunityTopic the same as Group (implicit-global candidate) via
// isGroupLikeChannelType, which diverged from production:
//
//   - Prod findActiveGrantsForChannel uses an INNER JOIN on obo_scopes
//     for ALL channel types (DM, Group, CommunityTopic), so a topic
//     without a scope row returns zero grants.
//   - Prod findGlobalGrantsWithoutScope is only invoked from
//     fanoutForMessage when channelType == ChannelTypeGroup, so the
//     implicit-scope path is unreachable for topics.
//
// Aligning the fake to that contract closes the divergence without
// expanding production code surface. CommunityTopic implicit-scope
// support is NOT planned; if that changes, both prod and the fake
// must be updated together.
//
// The original test (TestFindActiveGrantsForChannel_YUJ1538_
// CommunityTopicReturnsGlobalEnabled) asserted the inverse and is
// replaced by this regression — a refactor that re-introduces the
// fake-only topic implicit-scope branch surfaces here.
func TestFindActiveGrantsForChannel_PR121R6_CommunityTopicRequiresScopeRow(t *testing.T) {
	s := seedGrantNoScope(t)
	grants, err := s.findActiveGrantsForChannel("group_42____topic_a1", common.ChannelTypeCommunityTopic.Uint8())
	if err != nil {
		t.Fatalf("findActiveGrantsForChannel community-topic: %v", err)
	}
	if len(grants) != 0 {
		t.Fatalf("community-topic lookup must require a scope row (prod parity), got %+v", grants)
	}
}

// TestFindActiveGrantsForChannel_YUJ1538_GroupSkipsGloballyDisabled —
// store-level pin: the channel-type-aware branch must still respect
// the `global_enabled=0` kill switch. A grant flipped off via PUT
// /v1/obo/grants/:id must NOT surface even on the group path.
func TestFindActiveGrantsForChannel_YUJ1538_GroupSkipsGloballyDisabled(t *testing.T) {
	s := newFakeOBOStore()
	gid, err := s.insertGrant(tGrantor, tBot, "auto", "")
	if err != nil {
		t.Fatalf("insertGrant: %v", err)
	}
	// NB: insertGrant defaults global_enabled=0, and we intentionally
	// do NOT flip it on here — that's the case under test.
	_ = gid
	grants, err := s.findActiveGrantsForChannel("group_42", common.ChannelTypeGroup.Uint8())
	if err != nil {
		t.Fatalf("findActiveGrantsForChannel: %v", err)
	}
	if len(grants) != 0 {
		t.Fatalf("global_enabled=0 grant must NOT surface for groups, got %+v", grants)
	}
}

// TestDecodeMentionGate_YUJ1538_AllFlagShapes — exhaustive shape
// coverage for the `mention.all` truthy decoder. WuKongIM clients in
// the wild send `1` (number) and `true` (boolean); the legacy SDKs
// send `json.Number("1")` once the read path opts into UseNumber. The
// gate must accept all three and reject everything else (including
// `0`, `false`, missing, null, and unrelated types like strings).
func TestDecodeMentionGate_YUJ1538_AllFlagShapes(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		wantAll bool
	}{
		{"missing mention", `{"type":1}`, false},
		{"missing all", `{"mention":{"uids":["u"]}}`, false},
		{"all_number_one", `{"mention":{"all":1}}`, true},
		{"all_number_zero", `{"mention":{"all":0}}`, false},
		{"all_bool_true", `{"mention":{"all":true}}`, true},
		{"all_bool_false", `{"mention":{"all":false}}`, false},
		{"all_string_one", `{"mention":{"all":"1"}}`, false}, // strings are not truthy
		{"all_null", `{"mention":{"all":null}}`, false},
		{"mention_not_object", `{"mention":"@everyone"}`, false},
		{"payload_not_object", `42`, false},
		{"payload_empty", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, all, _, _ := decodeMentionGate([]byte(tc.payload))
			if all != tc.wantAll {
				t.Fatalf("payload %q: want all=%v, got %v", tc.payload, tc.wantAll, all)
			}
		})
	}
}

// ---------------------------------------------------------------------
// PR#114 review fix — checkOBO scope-row bypass for group-like channels
// (Jerry-Xin / lml2468). Pre-fix, the bot's OBO reply hit
// `store.scopeEnabled(...)` and returned false because operators never
// installed channel_type=2 scopes in production, so the reply 403'd
// even though the v2 fan-out trigger query had already been widened.
// ---------------------------------------------------------------------

// newBotAPIForCheckYUJ1538 mirrors newBotAPIWithFakeStore in
// obo_check_test.go but is duplicated here so the new tests live next
// to the rest of the YUJ-1538 pinning. The channel-access override
// defaults to "always allowed" so the assertions focus on the
// scope-row contract, not the TOCTOU re-check layer (which has its
// own dedicated test in obo_check_test.go).
func newBotAPIForCheckYUJ1538(s *fakeOBOStore) *BotAPI {
	return &BotAPI{
		Log:              log.NewTLog("BotAPI-yuj1538-check"),
		oboStoreOverride: s,
		oboChannelAccessOverride: func(uid, channelID string, channelType uint8) (bool, error) {
			return true, nil
		},
	}
}

// TestCheckOBO_YUJ1538_GroupNoScopeRow_GlobalEnabledAuthorizes — PR#114
// review blocker. With `global_enabled=1` and NO `obo_scopes` row,
// `checkOBO` for a Group channel must succeed (return nil). Pre-fix
// this returned ErrOBONotAuthorized because `scopeEnabled` was called
// unconditionally and answered false, so the bot's OBO reply 403'd
// even though PR#109 had already allowed the fan-out copy to reach
// the bot. The new branch in checkOBO skips scopeEnabled for
// group-like channel types; symmetry with findActiveGrantsForChannel.
func TestCheckOBO_YUJ1538_GroupNoScopeRow_GlobalEnabledAuthorizes(t *testing.T) {
	s := seedGrantNoScope(t)
	ba := newBotAPIForCheckYUJ1538(s)
	if err := ba.checkOBO(tBot, tGrantor, "group_42", common.ChannelTypeGroup.Uint8()); err != nil {
		t.Fatalf("YUJ-1538 / PR#114: group with global_enabled=1 and no scope row must authorize, got %v", err)
	}
}

// TestCheckOBO_YUJ1538_CommunityTopicNoScopeRow_GlobalEnabledAuthorizes —
// CommunityTopic shares the group-like "@grantor narrowing" contract
// and must therefore also bypass the scope-row requirement when
// `global_enabled=1`. Keeps the two channel types as separate test
// cases so a regression that drops only one surfaces with a precise
// failure message.
func TestCheckOBO_YUJ1538_CommunityTopicNoScopeRow_GlobalEnabledAuthorizes(t *testing.T) {
	s := seedGrantNoScope(t)
	ba := newBotAPIForCheckYUJ1538(s)
	if err := ba.checkOBO(tBot, tGrantor, "group_42____topic_a1", common.ChannelTypeCommunityTopic.Uint8()); err != nil {
		t.Fatalf("YUJ-1538 / PR#114: community-topic with global_enabled=1 and no scope row must authorize, got %v", err)
	}
}

// TestCheckOBO_YUJ1538_DMNoScope_StillUnauthorized — read/write symmetry
// pin. Originally this test asserted that DM with `global_enabled=1` and
// no scope row must DENY (the YUJ-1538-era intent). PR#162 inverted that
// invariant on the read path (`findGlobalGrantsForDM` delivers the inbound
// DM under the same predicate as the group implicit-scope feeder), and
// the follow-up (Mininglamp-OSS/octo-server#162 R1) extended `checkOBO`
// to honor the same predicate on the write path — otherwise the bot
// receives the DM via fan-out but its reply 403s.
//
// The test name is preserved so the historical YUJ-1538 pin stays
// findable in git blame; the assertion is now flipped to APPROVE the
// reply when the friend-gate (`grantorCanReadChannel` →
// `oboChannelAccessOverride`, defaulted to true in
// `newBotAPIForCheckYUJ1538`) confirms live access. The companion
// regression test `TestCheckOBO_DM_GlobalEnabled_NoScope_FriendGateDenied`
// pins the deny side: friend-gate denial must still block the reply.
func TestCheckOBO_YUJ1538_DMNoScope_StillUnauthorized(t *testing.T) {
	const dmPeer = "u_bob"
	s := seedGrantNoScope(t) // grant exists with global_enabled=1, but no scope
	ba := newBotAPIForCheckYUJ1538(s)
	if err := ba.checkOBO(tBot, tGrantor, dmPeer, common.ChannelTypePerson.Uint8()); err != nil {
		t.Fatalf("Mininglamp-OSS/octo-server#162 R1: DM with global_enabled=1 and no scope row must APPROVE when friend-gate allows access (read/write symmetry), got %v", err)
	}
}

// TestCheckOBO_DM_GlobalEnabled_NoScope_FriendGateDenied — deny-side
// regression for the PR#162 R1 DM implicit-scope branch. With
// `global_enabled=1`, no scope row, but the friend-gate
// (`grantorCanReadChannel` → `IsFriend`) returning false, the reply
// MUST still 403. This pins that the friend-gate is the load-bearing
// safety net: removing it (or letting an error fall through to "ok")
// would let an unrelated peer's DM authorize a reply, because there
// is no per-peer scope row in the picture.
func TestCheckOBO_DM_GlobalEnabled_NoScope_FriendGateDenied(t *testing.T) {
	const dmPeer = "u_stranger"
	s := seedGrantNoScope(t) // grant exists with global_enabled=1, but no scope
	ba := newBotAPIForCheckYUJ1538(s)
	// Friend-gate denies access for this peer.
	ba.oboChannelAccessOverride = func(uid, channelID string, channelType uint8) (bool, error) {
		if channelType == common.ChannelTypePerson.Uint8() && channelID == dmPeer {
			return false, nil
		}
		return true, nil
	}
	err := ba.checkOBO(tBot, tGrantor, dmPeer, common.ChannelTypePerson.Uint8())
	if !errors.Is(err, ErrOBONotAuthorized) {
		t.Fatalf("Mininglamp-OSS/octo-server#162 R1: DM with global_enabled=1 and no scope row must STILL deny when friend-gate returns false, got %v", err)
	}
}

// TestCheckOBO_YUJ1538_GroupGlobalDisabled_StillUnauthorized — the
// scope-row bypass only triggers when the grant itself is
// `global_enabled=1`. A group inbound for a grantor whose grant has
// the master switch OFF must still deny. Without this pin, a future
// refactor that moves the bypass above the `findActiveGrantByGrantorBot`
// gate would silently re-open the kill switch.
func TestCheckOBO_YUJ1538_GroupGlobalDisabled_StillUnauthorized(t *testing.T) {
	s := newFakeOBOStore()
	if _, err := s.insertGrant(tGrantor, tBot, "auto", ""); err != nil {
		t.Fatalf("insertGrant: %v", err)
	}
	// global_enabled stays 0 (insertGrant default).
	ba := newBotAPIForCheckYUJ1538(s)
	err := ba.checkOBO(tBot, tGrantor, "group_42", common.ChannelTypeGroup.Uint8())
	if !errors.Is(err, ErrOBONotAuthorized) {
		t.Fatalf("YUJ-1538 / PR#114: group with global_enabled=0 must STILL deny, got %v", err)
	}
}
