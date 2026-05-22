// Package bot_api · YUJ-1709 / Mininglamp-OSS/octo-server#125 — fan-out
// trigger must treat `mention.humans=1` (Plan X web client @所有人 shape)
// as equivalent to `mention.all=1` (legacy WuKongIM @所有人 shape).
//
// The pre-fix bug:
//
//   - `decodeMentionGate` only inspected `mention.all`. The Plan X web
//     client emits `mention: { humans: 1 }` for @所有人 and never sets
//     `mention.all`. The fan-out narrowing gate therefore early-returned
//     with `mentionAll=false` and zero uids, so any grantee bot for a
//     grantor present in the group silently missed the OBO DM.
//
// These tests pin the corrected behavior at the unit level — the
// `mention.humans` codepaths are exercised both via the pure decoder
// (TestDecodeMentionGate_YUJ1709_HumansFlagShapes) and via the
// end-to-end `fanoutForMessage` entry point so a regression that drops
// either the gate widening or the safety properties surrounding it
// fails fast at unit-test time instead of slipping into E2E.
//
// Safety guard: the new `humans` branch only widens the gate's entry
// condition. The downstream grant DB lookup, Gate 1/2/3 dispatch loop,
// and scope/access checks are untouched, so:
//
//   - bots without an active grant still receive zero fan-out copies
//     (TestFanout_YUJ1709_HumansNoGrant_StillSkips);
//   - DM payloads, which never carry `mention.humans`, continue to use
//     the unfiltered scope-joined query (the existing
//     TestFanout_YUJ1538_DMStillRequiresScopeRow case in the YUJ-1538
//     file covers this contract and remains green under this change).
package bot_api

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
)

// TestDecodeMentionGate_YUJ1709_HumansFlagShapes — exhaustive shape
// coverage for the new `mention.humans` truthy decoder. Mirrors the
// YUJ-1538 `mention.all` shape table so the two flags stay in lock-step:
// numeric `1` and boolean `true` are truthy, everything else (`0`,
// `false`, missing, null, strings, unrelated types) is not.
func TestDecodeMentionGate_YUJ1709_HumansFlagShapes(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		wantAll bool
	}{
		{"humans_number_one", `{"mention":{"humans":1}}`, true},
		{"humans_number_zero", `{"mention":{"humans":0}}`, false},
		{"humans_bool_true", `{"mention":{"humans":true}}`, true},
		{"humans_bool_false", `{"mention":{"humans":false}}`, false},
		{"humans_string_one", `{"mention":{"humans":"1"}}`, false},
		{"humans_null", `{"mention":{"humans":null}}`, false},
		// Co-presence: all=0 + humans=1 → still truthy (Plan X never
		// co-emits but we must not let an explicit `all:0` veto a
		// truthy `humans`).
		{"all_zero_humans_one", `{"mention":{"all":0,"humans":1}}`, true},
		// Co-presence: all=1 + humans=0 → still truthy (legacy clients
		// that happen to also emit humans=0 must keep working).
		{"all_one_humans_zero", `{"mention":{"all":1,"humans":0}}`, true},
		// Plain `@AI` style payload: neither flag set, just uids →
		// mentionAll must remain false (the uid-narrowed query handles
		// dispatch).
		{"only_uids", `{"mention":{"uids":["u_alice"]}}`, false},
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

// TestFanout_YUJ1709_GroupMentionHumansSummonsPersona — the end-to-end
// repro. With a single active grant (global_enabled=1, no scope row),
// a Group message carrying the Plan X web client's `mention.humans=1`
// shape must dispatch exactly one OBO fan-out copy to the grantee bot.
//
// Pre-fix this returned 0 dispatches because `decodeMentionGate`
// ignored `mention.humans`, so the narrowing gate early-returned with
// `mentionAll=false` and no uids.
func TestFanout_YUJ1709_GroupMentionHumansSummonsPersona(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantNoScope(t)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	// `@所有人 hi` Plan X shape: mention.humans=1, no `mention.all`,
	// no uids array. This is the exact payload observed in im-test
	// prod for octo-server#125.
	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"@所有人 hi","mention":{"humans":1}}`),
	}
	if got := ba.fanoutForMessage(msg); got != 1 {
		t.Fatalf("YUJ-1709: @所有人 (mention.humans=1) in group must summon every grantor's persona, got %d", got)
	}
	if len(fc.copies) != 1 {
		t.Fatalf("expected 1 captured copy, got %d", len(fc.copies))
	}
	cp := fc.copies[0]
	if cp.FromUID != tGrantor {
		t.Fatalf("fan-out copy FromUID: want grantor %q, got %q", tGrantor, cp.FromUID)
	}
	if cp.ChannelID != tBot {
		t.Fatalf("fan-out copy ChannelID: want bot %q, got %q", tBot, cp.ChannelID)
	}
}

// TestFanout_YUJ1709_GroupMentionHumansBooleanShape — defensive parity
// with TestFanout_YUJ1538_GroupMentionAllBooleanShape. Some clients
// may emit `mention.humans=true` (boolean) instead of the numeric `1`.
// The gate must honor both shapes.
func TestFanout_YUJ1709_GroupMentionHumansBooleanShape(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantNoScope(t)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"@所有人 hi","mention":{"humans":true}}`),
	}
	if got := ba.fanoutForMessage(msg); got != 1 {
		t.Fatalf("YUJ-1709: mention.humans=true (boolean) must be treated as truthy, got %d", got)
	}
}

// TestFanout_YUJ1709_GroupMentionHumansAllZero — the realistic Plan X
// shape is `mention: {humans: 1}` only, but a paranoid client could
// also emit `mention.all=0` alongside `humans=1`. An explicit zero
// `all` must NOT veto a truthy `humans`.
func TestFanout_YUJ1709_GroupMentionHumansAllZero(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantNoScope(t)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"@所有人","mention":{"all":0,"humans":1}}`),
	}
	if got := ba.fanoutForMessage(msg); got != 1 {
		t.Fatalf("YUJ-1709: mention.all=0 must not veto mention.humans=1, got %d", got)
	}
}

// TestFanout_YUJ1709_HumansNoGrant_StillSkips — safety guard. The new
// `humans` branch only widens the gate's entry condition; bots without
// an active grant for the grantor in the channel must still receive
// zero fan-out copies. Mirrors TestFanout_YUJ1538_GroupNoGrantsRegistered_StillSkips.
//
// Note: relies on the empty fake store returning zero grants for any
// channel lookup. The gate now widens to admit `humans=1` traffic, but
// findActiveGrantsForChannel still gates the actual dispatch.
func TestFanout_YUJ1709_HumansNoGrant_StillSkips(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := newFakeOBOStore() // NO grants installed
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"@所有人 hi","mention":{"humans":1}}`),
	}
	if got := ba.fanoutForMessage(msg); got != 0 {
		t.Fatalf("YUJ-1709: no grants registered must yield 0 fan-out copies even with mention.humans=1, got %d", got)
	}
	if len(fc.copies) != 0 {
		t.Fatalf("expected 0 captured copies, got %d", len(fc.copies))
	}
}

// TestFanout_YUJ1709_NoMentionHumansFalse_StillSkips — regression
// guard. A payload that explicitly sets `mention.humans=0` (and no
// other summon signal) must NOT trigger fan-out. Pins the negative
// direction of the new branch.
func TestFanout_YUJ1709_NoMentionHumansFalse_StillSkips(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantNoScope(t)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"just chatting","mention":{"humans":0}}`),
	}
	if got := ba.fanoutForMessage(msg); got != 0 {
		t.Fatalf("YUJ-1709: mention.humans=0 must NOT trigger fan-out, got %d", got)
	}
}
