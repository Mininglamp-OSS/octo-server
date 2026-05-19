// Package bot_api · YUJ-1166 — Unit tests for checkOBO.
//
// Coverage matrix (per task spec):
//   - happy path: active grant + matching scope → nil
//   - unauthorized: missing grant, revoked grant, global_enabled=0,
//     scope missing, scope disabled, self-grant attempt, empty inputs
//   - DB error: propagates upstream (caller responsible for 500)
package bot_api

import (
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
)

const (
	tBot     = "bot_clone_001"
	tGrantor = "user_yu"
	tChan    = "group_42"
)

func newBotAPIWithFakeStore(s *fakeOBOStore) *BotAPI {
	return &BotAPI{
		Log:              log.NewTLog("BotAPI-obo-test"),
		oboStoreOverride: s,
	}
}

// TestCheckOBO_Happy verifies the canonical authorized path: active grant
// (active=1, global_enabled=1) + matching enabled scope → nil.
func TestCheckOBO_Happy(t *testing.T) {
	s := newFakeOBOStore()
	gid, err := s.insertGrant(tGrantor, tBot, "auto")
	if err != nil {
		t.Fatalf("insertGrant: %v", err)
	}
	enable := 1
	if err := s.updateGrant(gid, "", &enable); err != nil {
		t.Fatalf("updateGrant: %v", err)
	}
	if _, err := s.insertScope(gid, tChan, common.ChannelTypeGroup.Uint8(), 1); err != nil {
		t.Fatalf("insertScope: %v", err)
	}

	ba := newBotAPIWithFakeStore(s)
	if err := ba.checkOBO(tBot, tGrantor, tChan, common.ChannelTypeGroup.Uint8()); err != nil {
		t.Fatalf("expected nil (authorized), got %v", err)
	}
}

// TestCheckOBO_NoGrant — no row at all for (grantor, bot).
func TestCheckOBO_NoGrant(t *testing.T) {
	ba := newBotAPIWithFakeStore(newFakeOBOStore())
	err := ba.checkOBO(tBot, tGrantor, tChan, common.ChannelTypeGroup.Uint8())
	if !errors.Is(err, ErrOBONotAuthorized) {
		t.Fatalf("want ErrOBONotAuthorized, got %v", err)
	}
}

// TestCheckOBO_GrantRevoked — row exists but active=0. Indistinguishable
// from "never existed" by contract.
func TestCheckOBO_GrantRevoked(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tGrantor, tBot, "auto")
	enable := 1
	_ = s.updateGrant(gid, "", &enable)
	_, _ = s.insertScope(gid, tChan, common.ChannelTypeGroup.Uint8(), 1)
	if err := s.revokeGrant(gid); err != nil {
		t.Fatalf("revokeGrant: %v", err)
	}

	ba := newBotAPIWithFakeStore(s)
	err := ba.checkOBO(tBot, tGrantor, tChan, common.ChannelTypeGroup.Uint8())
	if !errors.Is(err, ErrOBONotAuthorized) {
		t.Fatalf("revoked grant should be unauthorized, got %v", err)
	}
}

// TestCheckOBO_GlobalDisabled — active=1 but global_enabled=0 (the master
// kill-switch). Same denial behavior as no-grant.
func TestCheckOBO_GlobalDisabled(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tGrantor, tBot, "auto")
	// Skip the enable step → global_enabled stays 0.
	_, _ = s.insertScope(gid, tChan, common.ChannelTypeGroup.Uint8(), 1)

	ba := newBotAPIWithFakeStore(s)
	err := ba.checkOBO(tBot, tGrantor, tChan, common.ChannelTypeGroup.Uint8())
	if !errors.Is(err, ErrOBONotAuthorized) {
		t.Fatalf("global-off grant should be unauthorized, got %v", err)
	}
}

// TestCheckOBO_ScopeMissing — grant is fully enabled but no scope row
// for the channel. Whitelist semantics: channels not explicitly added
// MUST be denied (RFC §2).
func TestCheckOBO_ScopeMissing(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tGrantor, tBot, "auto")
	enable := 1
	_ = s.updateGrant(gid, "", &enable)
	// No scope inserted at all.

	ba := newBotAPIWithFakeStore(s)
	err := ba.checkOBO(tBot, tGrantor, tChan, common.ChannelTypeGroup.Uint8())
	if !errors.Is(err, ErrOBONotAuthorized) {
		t.Fatalf("missing scope should be unauthorized, got %v", err)
	}
}

// TestCheckOBO_ScopeDisabled — scope row exists with enabled=0.
func TestCheckOBO_ScopeDisabled(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tGrantor, tBot, "auto")
	enable := 1
	_ = s.updateGrant(gid, "", &enable)
	_, _ = s.insertScope(gid, tChan, common.ChannelTypeGroup.Uint8(), 0)

	ba := newBotAPIWithFakeStore(s)
	err := ba.checkOBO(tBot, tGrantor, tChan, common.ChannelTypeGroup.Uint8())
	if !errors.Is(err, ErrOBONotAuthorized) {
		t.Fatalf("disabled scope should be unauthorized, got %v", err)
	}
}

// TestCheckOBO_SelfGrantRejected — a bot trying to represent itself
// must short-circuit with ErrOBONotAuthorized even if a (bogus) row
// happened to exist (which the REST POST handler also rejects).
func TestCheckOBO_SelfGrantRejected(t *testing.T) {
	ba := newBotAPIWithFakeStore(newFakeOBOStore())
	err := ba.checkOBO(tBot, tBot, tChan, common.ChannelTypeGroup.Uint8())
	if !errors.Is(err, ErrOBONotAuthorized) {
		t.Fatalf("self-grant should be unauthorized, got %v", err)
	}
}

// TestCheckOBO_EmptyInputs — defensive: empty bot, grantor, or channel
// short-circuits as unauthorized (callers shouldn't pass these but it
// happens with broken proxies / typos).
func TestCheckOBO_EmptyInputs(t *testing.T) {
	ba := newBotAPIWithFakeStore(newFakeOBOStore())
	cases := []struct{ bot, grantor, ch string }{
		{"", tGrantor, tChan},
		{tBot, "", tChan},
		{tBot, tGrantor, ""},
	}
	for _, tc := range cases {
		if err := ba.checkOBO(tc.bot, tc.grantor, tc.ch, common.ChannelTypeGroup.Uint8()); !errors.Is(err, ErrOBONotAuthorized) {
			t.Fatalf("empty input (%q,%q,%q) should be unauthorized, got %v", tc.bot, tc.grantor, tc.ch, err)
		}
	}
}

// TestCheckOBO_DBError_OnGrantLookup — store error propagates so the
// caller can 500. We do NOT translate DB errors to "unauthorized" because
// that would hide real outages.
func TestCheckOBO_DBError_OnGrantLookup(t *testing.T) {
	boom := errors.New("connection refused")
	s := newFakeOBOStore()
	s.failFindActiveGrant = boom

	ba := newBotAPIWithFakeStore(s)
	err := ba.checkOBO(tBot, tGrantor, tChan, common.ChannelTypeGroup.Uint8())
	if err == nil || errors.Is(err, ErrOBONotAuthorized) {
		t.Fatalf("expected raw DB error to propagate, got %v", err)
	}
}

// TestCheckOBO_DBError_OnScopeLookup — same propagation contract for the
// second store call.
func TestCheckOBO_DBError_OnScopeLookup(t *testing.T) {
	boom := errors.New("connection refused")
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tGrantor, tBot, "auto")
	enable := 1
	_ = s.updateGrant(gid, "", &enable)
	s.failScopeEnabled = boom

	ba := newBotAPIWithFakeStore(s)
	err := ba.checkOBO(tBot, tGrantor, tChan, common.ChannelTypeGroup.Uint8())
	if err == nil || errors.Is(err, ErrOBONotAuthorized) {
		t.Fatalf("expected raw DB error to propagate, got %v", err)
	}
}
