//go:build integration

package message

// DB-level lock-down for the dm_space_presence accessors (pkg/space).
//
// Reviewer request (#519, Jerry-Xin): the raw SQL in UpsertDMSpacePresence /
// DMSpacePresenceSet / DMSpacePresenceAnySet was only covered indirectly via the
// webhook→filter integration path. This test exercises each accessor directly
// against the real MySQL schema so its SQL contract is asserted on its own:
//
//   - UpsertDMSpacePresence: idempotent insert; last_timestamp advances by
//     GREATEST (a lower ts never lowers the stored value); empty channel/space
//     is a silent no-op (no row written).
//   - DMSpacePresenceSet: per-Space IN filter, set keyed by fake_channel_id;
//     empty ids / empty space short-circuit to an empty set without querying.
//   - DMSpacePresenceAnySet: has-a-row-in-ANY-Space DISTINCT set; the
//     "tracked but only in other Spaces" derivation the default-Space catch-all
//     relies on (AnySet minus Set(defaultSpace)).
//
//	go test -tags=integration ./modules/message/ -run TestDMSpacePresenceAccessors -v

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDMSpacePresenceAccessors(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)
	db := ctx.DB()

	// Self-namespaced keys so the test is independent of rows any other suite may
	// have left behind — CleanAllTables does not truncate dm_space_presence.
	const (
		fcA    = "dmpresacc_pair_a"
		fcB    = "dmpresacc_pair_b"
		fcC    = "dmpresacc_pair_c" // deliberately never written → absent everywhere
		spaceA = "dmpresacc_space_a"
		spaceB = "dmpresacc_space_b"
	)
	ids := []string{fcA, fcB, fcC}
	_, err := db.DeleteBySql("DELETE FROM dm_space_presence WHERE fake_channel_id IN ?", ids).Exec()
	require.NoError(t, err)

	// --- empty-input / empty-arg guards: return empty, never error, never write.
	set, err := spacepkg.DMSpacePresenceSet(db, nil, spaceA)
	require.NoError(t, err)
	assert.Empty(t, set, "Set: nil ids → empty set")

	set, err = spacepkg.DMSpacePresenceSet(db, ids, "")
	require.NoError(t, err)
	assert.Empty(t, set, "Set: empty spaceID → empty set")

	anySet, err := spacepkg.DMSpacePresenceAnySet(db, nil)
	require.NoError(t, err)
	assert.Empty(t, anySet, "AnySet: nil ids → empty set")

	require.NoError(t, spacepkg.UpsertDMSpacePresence(db, "", spaceA, 1), "Upsert: empty channel is a no-op")
	require.NoError(t, spacepkg.UpsertDMSpacePresence(db, fcA, "", 1), "Upsert: empty space is a no-op")
	anySet, err = spacepkg.DMSpacePresenceAnySet(db, ids)
	require.NoError(t, err)
	assert.Empty(t, anySet, "no-op upserts must not write any row")

	// --- seed: fcA present in spaceA(ts=100) and spaceB(ts=50); fcB in spaceB only.
	require.NoError(t, spacepkg.UpsertDMSpacePresence(db, fcA, spaceA, 100))
	require.NoError(t, spacepkg.UpsertDMSpacePresence(db, fcA, spaceB, 50))
	require.NoError(t, spacepkg.UpsertDMSpacePresence(db, fcB, spaceB, 10))

	// --- DMSpacePresenceSet: per-Space IN filter, keyed by fake_channel_id.
	inSpaceA, err := spacepkg.DMSpacePresenceSet(db, ids, spaceA)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{fcA: true}, inSpaceA, "spaceA holds only fcA")

	inSpaceB, err := spacepkg.DMSpacePresenceSet(db, ids, spaceB)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{fcA: true, fcB: true}, inSpaceB, "spaceB holds fcA + fcB")

	// --- DMSpacePresenceAnySet: any-Space membership; fcC never written → absent.
	anySet, err = spacepkg.DMSpacePresenceAnySet(db, ids)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{fcA: true, fcB: true}, anySet)

	// The "elsewhere-only" derivation the catch-all hides on: tracked in some
	// Space (AnySet) but NOT in the default Space (Set(default)).
	assert.True(t, anySet[fcB] && !inSpaceA[fcB], "fcB is tracked but absent from spaceA → elsewhere-only vs spaceA")
	assert.False(t, anySet[fcA] && !inSpaceA[fcA], "fcA is present in spaceA → NOT elsewhere-only vs spaceA")

	// --- GREATEST: a lower ts must not lower the stored value; a higher one advances it.
	require.NoError(t, spacepkg.UpsertDMSpacePresence(db, fcA, spaceA, 40))
	assert.Equal(t, int64(100), readPresenceTS(t, db, fcA, spaceA), "lower ts (40) keeps stored 100")
	require.NoError(t, spacepkg.UpsertDMSpacePresence(db, fcA, spaceA, 250))
	assert.Equal(t, int64(250), readPresenceTS(t, db, fcA, spaceA), "higher ts (250) advances the value")
}

// readPresenceTS reads last_timestamp for one (fake_channel_id, space_id) row,
// mirroring the slice-Load idiom the accessors use.
func readPresenceTS(t *testing.T, db *dbr.Session, fakeChannelID, spaceID string) int64 {
	t.Helper()
	var tss []int64
	_, err := db.SelectBySql(
		"SELECT last_timestamp FROM dm_space_presence WHERE fake_channel_id = ? AND space_id = ?",
		fakeChannelID, spaceID,
	).Load(&tss)
	require.NoError(t, err)
	require.Len(t, tss, 1, "exactly one row for (%s,%s)", fakeChannelID, spaceID)
	return tss[0]
}
