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

// TestFanout_YUJ1538_GroupMentionAllSummonsPersona — `@所有人`
// (`mention.all=1`) in a group must trigger fan-out for every
// `global_enabled=1` grantor, even when the grantor is not listed in
// `mention.uids`. Pre-YUJ-1538 the narrowing gate only inspected
// `mention.uids`, so broadcast messages silently never reached the
// persona — a more visible regression than the missing scope-row case
// because grantors typically install bot personas precisely to
// monitor broadcasts.
func TestFanout_YUJ1538_GroupMentionAllSummonsPersona(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantNoScope(t)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	// `@所有人 hi` shape: mention.all=1, no uids array. Real WuKongIM
	// payloads use `1` (json.Number/float64 after json.Unmarshal); the
	// gate also accepts `true` (boolean) for forward compat.
	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"@所有人 hi","mention":{"all":1}}`),
	}
	if got := ba.fanoutForMessage(msg); got != 1 {
		t.Fatalf("YUJ-1538: @所有人 (mention.all=1) in group must summon every grantor's persona, got %d", got)
	}
	if len(fc.copies) != 1 {
		t.Fatalf("expected 1 captured copy, got %d", len(fc.copies))
	}
}

// TestFanout_YUJ1538_GroupMentionAllBooleanShape — defensive check
// that the boolean shape `mention.all=true` (some clients) is also
// honoured, not just the numeric `1` form.
func TestFanout_YUJ1538_GroupMentionAllBooleanShape(t *testing.T) {
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
	if got := ba.fanoutForMessage(msg); got != 1 {
		t.Fatalf("YUJ-1538: mention.all=true (boolean) must be treated as truthy, got %d", got)
	}
}

// TestFanout_YUJ1538_DMStillRequiresScopeRow — the new
// channel-type-aware lookup path must NOT relax DM behavior. DMs
// remain strict: a grant without a matching `obo_scopes` row
// (channel_type=1, channel_id=peer uid) gets zero dispatches even
// when `global_enabled=1`. Pins the contract from the issue:
//
//   "Do NOT change DM fan-out behavior (must still require scope rows)"
func TestFanout_YUJ1538_DMStillRequiresScopeRow(t *testing.T) {
	const peer = "u_bob"
	ct := common.ChannelTypePerson.Uint8()
	s := seedGrantNoScope(t) // grant exists with global_enabled=1, but no DM scope
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     peer,
		ChannelID:   tGrantor, // DM listener-native view: ChannelID = receiver = grantor
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"hey, can we chat?"}`),
	}
	if got := ba.fanoutForMessage(msg); got != 0 {
		t.Fatalf("YUJ-1538: DM with no scope row must NOT fan out (issue spec: DM behavior unchanged), got %d", got)
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

// TestFindActiveGrantsForChannel_YUJ1538_CommunityTopicReturnsGlobalEnabled —
// store-level pin for the community-topic branch. Same shape as the
// group test above; kept separate so a regression that drops one of
// the two group-like channel types surfaces with a precise failure
// message.
func TestFindActiveGrantsForChannel_YUJ1538_CommunityTopicReturnsGlobalEnabled(t *testing.T) {
	s := seedGrantNoScope(t)
	grants, err := s.findActiveGrantsForChannel("group_42____topic_a1", common.ChannelTypeCommunityTopic.Uint8())
	if err != nil {
		t.Fatalf("findActiveGrantsForChannel community-topic: %v", err)
	}
	if len(grants) != 1 || grants[0].GrantorUID != tGrantor {
		t.Fatalf("community-topic lookup must return the global_enabled grant, got %+v", grants)
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
			_, all := decodeMentionGate([]byte(tc.payload))
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

// TestCheckOBO_YUJ1538_DMNoScope_StillUnauthorized — regression guard.
// The PR#114 fix MUST NOT relax DM behavior: a grant with
// `global_enabled=1` but no scope row for the DM peer must still deny.
// DMs have no in-message narrowing signal (mentions don't apply), so
// the per-peer scope row is the only explicit opt-in.
func TestCheckOBO_YUJ1538_DMNoScope_StillUnauthorized(t *testing.T) {
	const dmPeer = "u_bob"
	s := seedGrantNoScope(t) // grant exists with global_enabled=1, but no scope
	ba := newBotAPIForCheckYUJ1538(s)
	err := ba.checkOBO(tBot, tGrantor, dmPeer, common.ChannelTypePerson.Uint8())
	if !errors.Is(err, ErrOBONotAuthorized) {
		t.Fatalf("YUJ-1538 / PR#114: DM with no scope row must STILL deny (regression guard), got %v", err)
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
