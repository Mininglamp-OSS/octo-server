package space

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAtomicAddMember_ConcurrentJoinsNeverExceedCapacity pins the guard that the
// COUNT ... FOR UPDATE exists for, so the "skip it when unlimited" shortcut
// cannot be widened into "skip it always".
//
// Ten goroutines race to join a Space with two free seats. Without the lock the
// classic interleaving — every transaction reads the same count before any of
// them inserts — lets all ten in. Exactly two must win.
func TestAtomicAddMember_ConcurrentJoinsNeverExceedCapacity(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "sp-cap-race"
	const maxUsers = 3 // owner + 2 free seats
	seedInitialSpace(t, f, spaceID, maxUsers, JoinModeDirect)

	const racers = 10
	var wg sync.WaitGroup
	results := make([]error, racers)
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func(i int) {
			defer wg.Done()
			results[i] = f.db.atomicAddMemberIfNotFull(spaceID, uidForRacer(i), maxUsers)
		}(i)
	}
	wg.Wait()

	joined := 0
	for _, err := range results {
		if err == nil {
			joined++
		}
	}
	assert.Equal(t, 2, joined, "exactly the two free seats may be taken")

	var active int
	_, err = testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM space_member WHERE space_id=? AND status=1", spaceID).Load(&active)
	require.NoError(t, err)
	assert.Equal(t, maxUsers, active, "member count must never exceed max_users")
}

// TestAtomicAddMember_UnlimitedSpaceAcceptsConcurrentJoins is the other half:
// with max_users=0 there is no capacity to protect, so the count and its
// space-wide lock are skipped — and every concurrent join must still succeed.
//
// This is the case an OIDC initial Space is in, and the one that turns a sparse
// event into a burst on the first day of an SSO rollout: every employee's first
// login lands on the same space_id, on the login response path.
func TestAtomicAddMember_UnlimitedSpaceAcceptsConcurrentJoins(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "sp-cap-unlimited"
	seedInitialSpace(t, f, spaceID, 0, JoinModeDirect)

	const racers = 10
	var wg sync.WaitGroup
	results := make([]error, racers)
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func(i int) {
			defer wg.Done()
			results[i] = f.db.atomicAddMemberIfNotFull(spaceID, uidForRacer(i), 0)
		}(i)
	}
	wg.Wait()

	for i, err := range results {
		assert.NoErrorf(t, err, "racer %d must get in when the Space is unlimited", i)
	}

	var active int
	_, err = testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM space_member WHERE space_id=? AND status=1", spaceID).Load(&active)
	require.NoError(t, err)
	assert.Equal(t, racers+1, active, "all racers plus the owner")
}

// TestAtomicReactivateMember_RespectsCapacity keeps the sibling function honest:
// the same shortcut was applied there, and a rejoining ex-member must still be
// refused when the Space has filled up since they left.
func TestAtomicReactivateMember_RespectsCapacity(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "sp-cap-reactivate"
	seedInitialSpace(t, f, spaceID, 1, JoinModeDirect) // the owner fills it

	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceID, UID: "u-left", Role: 0, Status: 0,
	}))

	err = f.db.atomicReactivateMemberIfNotFull(spaceID, "u-left", 1)
	assert.ErrorIs(t, err, ErrSpaceFull, "a full Space must not take an ex-member back")

	m, qerr := f.db.queryMemberIncludeRemoved(spaceID, "u-left")
	require.NoError(t, qerr)
	require.NotNil(t, m)
	assert.Equal(t, 0, m.Status, "the refused reactivation must not have written")
}

// TestAtomicReactivateMember_UnlimitedSpaceLetsExMemberBack is the unlimited
// counterpart, so skipping the count cannot silently break the rejoin path.
func TestAtomicReactivateMember_UnlimitedSpaceLetsExMemberBack(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "sp-cap-reactivate-open"
	seedInitialSpace(t, f, spaceID, 0, JoinModeDirect)

	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceID, UID: "u-back", Role: 0, Status: 0,
	}))

	require.NoError(t, f.db.atomicReactivateMemberIfNotFull(spaceID, "u-back", 0))

	m, qerr := f.db.queryMemberIncludeRemoved(spaceID, "u-back")
	require.NoError(t, qerr)
	require.NotNil(t, m)
	assert.Equal(t, 1, m.Status)
	assert.Equal(t, 0, m.Role)
}

func uidForRacer(i int) string {
	return "u-racer-" + string(rune('a'+i))
}
