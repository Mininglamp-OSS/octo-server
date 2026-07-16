// Package bot_api · YUJ-53 — Unit tests for the SearchOBOAllowed adapter that
// exposes checkOBO to the messages_search as-user(OBO) gate. The adapter only
// remaps checkOBO's error contract into (bool, error); the underlying grant /
// scope / TOCTOU matrix is exercised by obo_check_test.go.
package bot_api

import (
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
)

// TestSearchOBOAllowed_Authorized — an active grant + enabled scope + live
// grantor access maps to (true, nil).
func TestSearchOBOAllowed_Authorized(t *testing.T) {
	s := newFakeOBOStore()
	gid, err := s.insertGrant(tGrantor, tBot, "auto", "")
	if err != nil {
		t.Fatalf("insertGrant: %v", err)
	}
	enable := 1
	if err := s.updateGrant(gid, "", &enable, nil); err != nil {
		t.Fatalf("updateGrant: %v", err)
	}
	if _, err := s.insertScope(gid, tChan, common.ChannelTypeGroup.Uint8(), 1); err != nil {
		t.Fatalf("insertScope: %v", err)
	}

	ba := newBotAPIWithFakeStore(s)
	ok, err := ba.SearchOBOAllowed(tBot, tGrantor, tChan, common.ChannelTypeGroup.Uint8())
	if err != nil {
		t.Fatalf("authorized: unexpected err %v", err)
	}
	if !ok {
		t.Fatalf("authorized: want ok=true")
	}
}

// TestSearchOBOAllowed_NotAuthorizedHidesExistence — ErrOBONotAuthorized
// (no grant here) collapses to (false, nil) so the search layer hides the
// channel rather than surfacing a distinguishable error.
func TestSearchOBOAllowed_NotAuthorizedHidesExistence(t *testing.T) {
	ba := newBotAPIWithFakeStore(newFakeOBOStore())
	ok, err := ba.SearchOBOAllowed(tBot, tGrantor, tChan, common.ChannelTypeGroup.Uint8())
	if err != nil {
		t.Fatalf("not-authorized must not surface an error, got %v", err)
	}
	if ok {
		t.Fatalf("not-authorized: want ok=false")
	}
}

// TestSearchOBOAllowed_InfraErrorPropagates — a genuine infra error (grantor
// live-access re-check fails) propagates as (false, err) so the caller can
// fail closed with INTERNAL rather than mistaking it for "not authorized".
func TestSearchOBOAllowed_InfraErrorPropagates(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tGrantor, tBot, "auto", "")
	enable := 1
	_ = s.updateGrant(gid, "", &enable, nil)
	_, _ = s.insertScope(gid, tChan, common.ChannelTypeGroup.Uint8(), 1)

	ba := newBotAPIWithFakeStore(s)
	sentinel := errors.New("db down")
	ba.oboChannelAccessOverride = func(uid, channelID string, channelType uint8) (bool, error) {
		return false, sentinel
	}
	ok, err := ba.SearchOBOAllowed(tBot, tGrantor, tChan, common.ChannelTypeGroup.Uint8())
	if !errors.Is(err, sentinel) {
		t.Fatalf("infra error must propagate, got %v", err)
	}
	if ok {
		t.Fatalf("infra error: want ok=false")
	}
}
