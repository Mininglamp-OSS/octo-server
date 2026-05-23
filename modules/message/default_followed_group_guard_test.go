package message

import (
	"errors"
	"testing"

	convext "github.com/Mininglamp-OSS/octo-server/modules/conversation_ext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Stage-1 / Stage-2 unit coverage for defaultFollowedGroupGuard
//
// Issue #151 re-review M4: the spaceID regression test at the service layer
// uses a coarse "allow / reject" stub guard — it does not exercise the actual
// two-stage code path in defaultFollowedGroupGuard.FilterDefaultFollowed.
// These tests substitute fakes for both stages so every rejection reason
// (non-member / disbanded / wrong-Space / infra-error) is covered.
// ---------------------------------------------------------------------------

type fakeCategoryFilter struct {
	// allowed maps candidate group_no → "passes Stage 1".  Anything not in
	// the map is dropped (simulating: no category, soft-deleted category,
	// or wrong uid).
	allowed map[string]bool
	err     error
}

func (f *fakeCategoryFilter) FilterDefaultFollowedGroups(uid string, candidates []string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if f.allowed[c] {
			out = append(out, c)
		}
	}
	return out, nil
}

type fakeChannelAuth struct {
	// rejectWith[uid|spaceID|groupNo] is the error to return — typically
	// convext.ErrChannelForbidden for "drop silently" cases, or a custom
	// error for "propagate as infra failure" cases.  Missing entries → nil
	// (group passes Stage 2).
	rejectWith map[string]error
}

func (f *fakeChannelAuth) AuthorizeChannelFollow(uid, spaceID, groupNo string) error {
	if err, ok := f.rejectWith[uid+"|"+spaceID+"|"+groupNo]; ok {
		return err
	}
	return nil
}

func newGuard(cat *fakeCategoryFilter, auth *fakeChannelAuth) *defaultFollowedGroupGuard {
	return &defaultFollowedGroupGuard{db: cat, channelAuth: auth}
}

func TestDefaultFollowedGroupGuard_HappyPath(t *testing.T) {
	g := newGuard(
		&fakeCategoryFilter{allowed: map[string]bool{"g1": true, "g2": true}},
		&fakeChannelAuth{},
	)
	out, err := g.FilterDefaultFollowed("u1", "s1", []string{"g1", "g2"})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"g1", "g2"}, out)
}

func TestDefaultFollowedGroupGuard_Stage1DropsNonCategorized(t *testing.T) {
	g := newGuard(
		&fakeCategoryFilter{allowed: map[string]bool{"g-categorized": true}},
		&fakeChannelAuth{},
	)
	out, err := g.FilterDefaultFollowed("u1", "s1",
		[]string{"g-categorized", "g-no-category", "g-soft-deleted-cat"})
	require.NoError(t, err)
	assert.Equal(t, []string{"g-categorized"}, out,
		"only categorized groups survive Stage 1 — soft-deleted and "+
			"uncategorized are dropped before any channel auth round-trip")
}

func TestDefaultFollowedGroupGuard_Stage2DropsNonMember(t *testing.T) {
	const uid, space = "u-not-member", "s1"
	g := newGuard(
		&fakeCategoryFilter{allowed: map[string]bool{"g-private": true}},
		&fakeChannelAuth{rejectWith: map[string]error{
			uid + "|" + space + "|g-private": convext.ErrChannelForbidden,
		}},
	)
	out, err := g.FilterDefaultFollowed(uid, space, []string{"g-private"})
	require.NoError(t, err,
		"ErrChannelForbidden in Stage 2 must be silently dropped (no error "+
			"propagation) so the rest of the payload can still proceed")
	assert.Empty(t, out,
		"non-member group with category must NOT survive — would otherwise "+
			"leak thread metadata via OnThreadCreated fanout")
}

func TestDefaultFollowedGroupGuard_Stage2DropsDisbandedGroup(t *testing.T) {
	const uid, space = "u1", "s1"
	// The real checkChannelAccess returns ErrChannelForbidden for Disband
	// (per modules/message/1module.go:214).  We model that with the same
	// sentinel — the guard treats Disband identically to non-member.
	g := newGuard(
		&fakeCategoryFilter{allowed: map[string]bool{"g-disbanded": true}},
		&fakeChannelAuth{rejectWith: map[string]error{
			uid + "|" + space + "|g-disbanded": convext.ErrChannelForbidden,
		}},
	)
	out, err := g.FilterDefaultFollowed(uid, space, []string{"g-disbanded"})
	require.NoError(t, err)
	assert.Empty(t, out,
		"disbanded groups must not be materialized — otherwise a re-creation "+
			"of follower-fanout rows for a tombstoned group lands in the user's "+
			"ext table and surfaces in sidebar")
}

func TestDefaultFollowedGroupGuard_Stage2DropsCrossSpaceGroup(t *testing.T) {
	const uid, spaceA, spaceB = "u1", "sA", "sB"
	// User has category for g-cross from when they were in Space A.
	// In Space B, the channel auth returns ErrChannelForbidden because the
	// group is not visible (parentSpaceID == sA, no external-group fallback).
	g := newGuard(
		&fakeCategoryFilter{allowed: map[string]bool{"g-cross": true}},
		&fakeChannelAuth{rejectWith: map[string]error{
			uid + "|" + spaceB + "|g-cross": convext.ErrChannelForbidden,
		}},
	)
	out, err := g.FilterDefaultFollowed(uid, spaceB, []string{"g-cross"})
	require.NoError(t, err)
	assert.Empty(t, out,
		"group categorized in Space A must NOT survive guard when called "+
			"from Space B — issue #151 code review #2")

	// Sanity: same group + uid in Space A → no rejection → survives.
	gA := newGuard(
		&fakeCategoryFilter{allowed: map[string]bool{"g-cross": true}},
		&fakeChannelAuth{}, // no rejection for Space A
	)
	out, err = gA.FilterDefaultFollowed(uid, spaceA, []string{"g-cross"})
	require.NoError(t, err)
	assert.Equal(t, []string{"g-cross"}, out)
}

func TestDefaultFollowedGroupGuard_Stage2InfraErrorPropagates(t *testing.T) {
	wantErr := errors.New("group_db.ExistMember down")
	g := newGuard(
		&fakeCategoryFilter{allowed: map[string]bool{"g1": true}},
		&fakeChannelAuth{rejectWith: map[string]error{
			"u1|s1|g1": wantErr,
		}},
	)
	out, err := g.FilterDefaultFollowed("u1", "s1", []string{"g1"})
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr,
		"non-ErrChannelForbidden errors must propagate so the caller can "+
			"turn them into a 5xx instead of silently treating the group as "+
			"'unauthorized' and falling back to ErrSortTargetNotFound")
	assert.Nil(t, out, "no partial result must be returned on infra error")
}

func TestDefaultFollowedGroupGuard_Stage1ErrorPropagates(t *testing.T) {
	wantErr := errors.New("group_setting JOIN query failed")
	g := newGuard(
		&fakeCategoryFilter{err: wantErr},
		&fakeChannelAuth{},
	)
	out, err := g.FilterDefaultFollowed("u1", "s1", []string{"g1"})
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr,
		"Stage 1 DB errors must propagate verbatim — the caller bubbles them "+
			"up so the sort request fails loudly rather than silently dropping "+
			"all groups (which would surface as the misleading ErrSortTargetNotFound)")
	assert.Nil(t, out)
}

func TestDefaultFollowedGroupGuard_EmptyInput_SkipsBothStages(t *testing.T) {
	// Use stubs that would fail loudly if called, to assert short-circuit.
	cat := &fakeCategoryFilter{err: errors.New("stage 1 must not be called")}
	auth := &fakeChannelAuth{rejectWith: map[string]error{
		"any|any|any": errors.New("stage 2 must not be called"),
	}}
	g := newGuard(cat, auth)

	out, err := g.FilterDefaultFollowed("u1", "s1", nil)
	require.NoError(t, err)
	assert.Nil(t, out)

	out, err = g.FilterDefaultFollowed("u1", "s1", []string{})
	require.NoError(t, err)
	assert.Nil(t, out)
}

func TestDefaultFollowedGroupGuard_MixedPayload_PartialFiltering(t *testing.T) {
	const uid, space = "u1", "s1"
	g := newGuard(
		&fakeCategoryFilter{allowed: map[string]bool{
			"g-keep":         true, // survives both stages
			"g-no-member":    true, // survives Stage 1, dropped at Stage 2
			"g-also-keep":    true, // survives both stages
			"g-wrong-space":  true, // survives Stage 1, dropped at Stage 2
			// g-no-category is absent → Stage 1 drops it
		}},
		&fakeChannelAuth{rejectWith: map[string]error{
			uid + "|" + space + "|g-no-member":   convext.ErrChannelForbidden,
			uid + "|" + space + "|g-wrong-space": convext.ErrChannelForbidden,
		}},
	)
	out, err := g.FilterDefaultFollowed(uid, space, []string{
		"g-keep", "g-no-member", "g-also-keep", "g-wrong-space", "g-no-category",
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"g-keep", "g-also-keep"}, out,
		"only groups passing BOTH stages survive — mixed payloads with "+
			"some attacker-injected IDs and some legitimate ones produce the "+
			"legitimate-only subset, not an all-or-nothing rejection")
}
