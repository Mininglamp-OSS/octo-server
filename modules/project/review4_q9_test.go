package project

import (
	"errors"
	"strconv"
	"testing"
	"time"

	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Q9: the cascade step must be verifiably REGISTERED ---

// TestCascadeStepIsActuallyRegistered pins the registration itself, not just the step body.
//
// Before this test existed, every cascade test called the step function directly, so deleting
// p.registerSpaceMemberRemovalCleanup() from New() - unwiring the entire asynchronous half of
// invariant I1 - left the suite green. spacemod.MemberRemovalCleanupStepNames exposes the
// registry; this test fails if the step is missing.
func TestCascadeStepIsActuallyRegistered(t *testing.T) {
	names := spacemod.MemberRemovalCleanupStepNames()
	require.Contains(t, names, spaceMemberRemovalStepName,
		"the project cascade step must be registered under %q; without it a Space removal "+
			"never closes project seats and nothing notices", spaceMemberRemovalStepName)
}

// --- Q9: the reentrancy guard must be OBSERVABLE, not just race-silent ---

// TestReconcileGuardActuallySkipsTheSecondRun pins the guard with a counter: overlapping
// ticks must not double-run the scans. The earlier version of this test had no assertions at
// all ("the assertion is the absence of a race") — after epochHistory became mutex-protected,
// deleting the CompareAndSwap guard left it green, so it pinned nothing.
func TestReconcileGuardActuallySkipsTheSecondRun(t *testing.T) {
	srv, p := setup(t)
	resetCursorsForTest()

	// Each scan defers one histogram Observe, so the histogram's SUM grows by >=4 per executed
	// run and not at all for a skipped one. CollectAndCount would NOT work here — it counts
	// series, and repeated Observes add no series (the earlier version of this test had no
	// assertion at all; the counter-shaped one could not kill the mutation).
	histogramSum := func() float64 {
		mfs, err := prometheus.DefaultGatherer.Gather()
		require.NoError(t, err)
		for _, mf := range mfs {
			if mf.GetName() != "project_reconcile_duration_seconds" {
				continue
			}
			for _, m := range mf.GetMetric() {
				if m.GetHistogram() != nil {
					return m.GetHistogram().GetSampleSum()
				}
			}
		}
		return 0
	}

	before := histogramSum()
	p.runReconcile() // enters, runs
	afterFirst := histogramSum()
	assert.Greater(t, afterFirst, before, "the first run must execute the scans")

	reconcileRunning.Store(true) // simulate an in-flight run
	p.runReconcile()             // must skip immediately
	afterSecond := histogramSum()
	assert.Equal(t, afterFirst, afterSecond,
		"while the guard is held, a tick must not execute any scan")
	reconcileRunning.Store(false)
	_ = srv
}

// --- Q9: the middleware cache seams exist and were never exercised ---

// TestInvalidateProjectMemberCacheThreeBranches covers the seam the round-3 review noted was
// unexercised despite 15 lines of design comment: DEL success (no-op), DEL failure + negative
// fallback written (ErrProjectCacheNegativeFallback), and BOTH failing (a real
// "boundary may have moved" error).
type fakeCacheStore struct {
	delErr        error
	setErr        error
	delCalls      int
	setCalls      int
	lastSetValue  string
	lastSetExpiry bool
}

func (f *fakeCacheStore) Del(key string) error {
	f.delCalls++
	return f.delErr
}

func (f *fakeCacheStore) SetAndExpire(key string, value interface{}, expire time.Duration) error {
	f.setCalls++
	f.lastSetValue, _ = value.(string)
	f.lastSetExpiry = expire > 0
	return f.setErr
}

func TestInvalidateProjectMemberCacheThreeBranches(t *testing.T) {
	// DEL succeeds: nothing else happens.
	store := &fakeCacheStore{}
	require.NoError(t, invalidateProjectMemberCacheIn(store, "p1", "u1"))
	assert.Equal(t, 1, store.delCalls)
	assert.Equal(t, 0, store.setCalls)

	// DEL fails, fallback lands: the boundary HELD (a negative entry shadows the stale
	// positive one); the error is the sentinel, so callers report it as held-but-dirty.
	store = &fakeCacheStore{delErr: errors.New("del down")}
	err := invalidateProjectMemberCacheIn(store, "p1", "u1")
	require.ErrorIs(t, err, errProjectCacheNegativeFallback)
	assert.Equal(t, strconv.Itoa(roleNonMember), store.lastSetValue,
		"the fallback must write the not-a-member value")
	assert.True(t, store.lastSetExpiry)

	// Both fail: a real authorization-failure signal.
	store = &fakeCacheStore{delErr: errors.New("del down"), setErr: errors.New("set down")}
	err = invalidateProjectMemberCacheIn(store, "p1", "u1")
	require.Error(t, err)
	assert.NotErrorIs(t, err, errProjectCacheNegativeFallback)
}

// TestCachedProjectRoleFallsBackOnCorruptValue pins the corrupt-cache path: a garbage value
// in project:member:* must fall through to the database, never be parsed as role 0
// (RoleCommon) — which would hand a non-member ordinary-member rights.
func TestCachedProjectRoleFallsBackOnCorruptValue(t *testing.T) {
	srv, p := setup(t)
	_, _, created := projectWithMembers(t, srv)

	conn := p.ctx.GetRedisConn()
	require.NotNil(t, conn)
	key := projectMemberCacheKey(created.ProjectID, "attacker")
	require.NoError(t, conn.SetAndExpire(key, "not-a-number", time.Minute))

	role, err := p.cachedProjectRole(created.ProjectID, "attacker")
	require.NoError(t, err)
	assert.Equal(t, roleNonMember, role,
		"a corrupt cached role must be discarded, not treated as RoleCommon")
}
