package common

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSpaceWelcomeConfigStore_CRUD exercises the per-Space config store end to
// end against a real DB (task space-welcome-per-space-admin-crud).
func TestSpaceWelcomeConfigStore_CRUD(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })

	store := NewSpaceWelcomeConfigStore(ctx.DB())
	bg := context.Background()
	now := time.Now().UTC()

	// Absent -> found=false, no error.
	_, found, err := store.Get(bg, "spc_1")
	require.NoError(t, err)
	assert.False(t, found)
	exists, err := store.Exists(bg, "spc_1")
	require.NoError(t, err)
	assert.False(t, exists)

	// Insert.
	require.NoError(t, store.Upsert(bg, SpaceWelcomeSpaceConfig{
		SpaceID:       "spc_1",
		Enabled:       true,
		ActiveFromRaw: "2026-07-10T00:00:00Z",
		Message:       "欢迎\n多行",
		UpdatedBy:     "u_admin",
	}, now))

	row, found, err := store.Get(bg, "spc_1")
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, row.Enabled)
	assert.Equal(t, "2026-07-10T00:00:00Z", row.ActiveFromRaw)
	assert.Equal(t, "欢迎\n多行", row.Message, "internal newline preserved verbatim")
	assert.Equal(t, "u_admin", row.UpdatedBy)
	exists, err = store.Exists(bg, "spc_1")
	require.NoError(t, err)
	assert.True(t, exists)

	// Update (upsert on the same space): flip enabled, change body, clear active_from.
	require.NoError(t, store.Upsert(bg, SpaceWelcomeSpaceConfig{
		SpaceID:       "spc_1",
		Enabled:       false,
		ActiveFromRaw: "",
		Message:       "updated",
		UpdatedBy:     "u_admin2",
	}, now.Add(time.Minute)))
	row, _, err = store.Get(bg, "spc_1")
	require.NoError(t, err)
	assert.False(t, row.Enabled)
	assert.Equal(t, "", row.ActiveFromRaw, "empty active_from stored as empty string")
	assert.Equal(t, "updated", row.Message)
	assert.Equal(t, "u_admin2", row.UpdatedBy)

	// ListEnabled returns only enabled rows.
	require.NoError(t, store.Upsert(bg, SpaceWelcomeSpaceConfig{
		SpaceID: "spc_2", Enabled: true, ActiveFromRaw: "2026-07-11T00:00:00Z", Message: "hi2", UpdatedBy: "u_a",
	}, now))
	list, err := store.ListEnabled(bg)
	require.NoError(t, err)
	got := map[string]bool{}
	for _, c := range list {
		got[c.SpaceID] = true
	}
	assert.True(t, got["spc_2"], "enabled spc_2 listed")
	assert.False(t, got["spc_1"], "disabled spc_1 must not be listed")

	// Delete is idempotent and reverts the space to unconfigured.
	deleted, err := store.Delete(bg, "spc_1")
	require.NoError(t, err)
	assert.True(t, deleted)
	deleted, err = store.Delete(bg, "spc_1")
	require.NoError(t, err)
	assert.False(t, deleted, "deleting an absent row is a no-op success")
	_, found, err = store.Get(bg, "spc_1")
	require.NoError(t, err)
	assert.False(t, found)
}

// TestSpaceWelcomeConfigStore_UpsertMerged_NoLostUpdate is the concurrent
// partial-update regression (reviewer-requested). Two admins patch DIFFERENT
// fields of the same Space simultaneously. UpsertMerged reads under a row lock
// (SELECT ... FOR UPDATE), so whichever transaction runs second merges onto the
// first's committed write — the final row must reflect BOTH patches regardless
// of interleaving. Without the lock, one admin's field would be silently lost.
func TestSpaceWelcomeConfigStore_UpsertMerged_NoLostUpdate(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })

	store := NewSpaceWelcomeConfigStore(ctx.DB())
	bg := context.Background()

	// Seed the enabled config both admins start from.
	require.NoError(t, store.Upsert(bg, SpaceWelcomeSpaceConfig{
		SpaceID:       "spc_race",
		Enabled:       true,
		ActiveFromRaw: "2026-07-10T00:00:00Z",
		Message:       "orig",
		UpdatedBy:     "seed",
	}, time.Now().UTC()))

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	// Admin A: change only the message, preserving whatever enabled it reads.
	go func() {
		defer wg.Done()
		<-start
		_, err := store.UpsertMerged(bg, "spc_race", time.Now().UTC(),
			func(cur *SpaceWelcomeSpaceConfig, found bool) (SpaceWelcomeSpaceConfig, error) {
				next := SpaceWelcomeSpaceConfig{SpaceID: "spc_race"}
				if cur != nil {
					next = *cur
				}
				next.Message = "msg-A"
				next.UpdatedBy = "admin_A"
				return next, nil
			})
		assert.NoError(t, err)
	}()

	// Admin B: disable, preserving whatever message it reads.
	go func() {
		defer wg.Done()
		<-start
		_, err := store.UpsertMerged(bg, "spc_race", time.Now().UTC(),
			func(cur *SpaceWelcomeSpaceConfig, found bool) (SpaceWelcomeSpaceConfig, error) {
				next := SpaceWelcomeSpaceConfig{SpaceID: "spc_race"}
				if cur != nil {
					next = *cur
				}
				next.Enabled = false
				next.UpdatedBy = "admin_B"
				return next, nil
			})
		assert.NoError(t, err)
	}()

	close(start) // release both goroutines to contend on the row lock
	wg.Wait()

	row, found, err := store.Get(bg, "spc_race")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "msg-A", row.Message, "admin A's message must not be lost")
	assert.False(t, row.Enabled, "admin B's disable must not be lost")
}

// TestSpaceWelcomeConfigStore_UpsertMerged_ConcurrentCreate is the concurrent
// FIRST-create regression (reviewer-requested): two admins issue the first PUT
// for the SAME absent config at once, each patching a different field. A plain
// SELECT ... FOR UPDATE cannot lock a not-yet-existing row, so without the
// insert-then-lock strategy both would merge onto defaults and the later write
// would clobber the earlier's field. With it, the second create serializes on
// the row the first inserted and merges onto its committed state — both fields
// survive and there is exactly one row (no duplicate, no error).
func TestSpaceWelcomeConfigStore_UpsertMerged_ConcurrentCreate(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })

	store := NewSpaceWelcomeConfigStore(ctx.DB())
	bg := context.Background()

	// No seed row: this is a first-time create race for an ABSENT config.
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	// Admin A: create with a message only (leaves enabled at default false).
	go func() {
		defer wg.Done()
		<-start
		_, err := store.UpsertMerged(bg, "spc_new", time.Now().UTC(),
			func(cur *SpaceWelcomeSpaceConfig, found bool) (SpaceWelcomeSpaceConfig, error) {
				next := SpaceWelcomeSpaceConfig{SpaceID: "spc_new"}
				if cur != nil {
					next = *cur
				}
				next.Message = "msg-A"
				next.UpdatedBy = "admin_A"
				return next, nil
			})
		assert.NoError(t, err)
	}()

	// Admin B: create with enabled=true only (leaves message at default "").
	go func() {
		defer wg.Done()
		<-start
		_, err := store.UpsertMerged(bg, "spc_new", time.Now().UTC(),
			func(cur *SpaceWelcomeSpaceConfig, found bool) (SpaceWelcomeSpaceConfig, error) {
				next := SpaceWelcomeSpaceConfig{SpaceID: "spc_new"}
				if cur != nil {
					next = *cur
				}
				next.Enabled = true
				next.UpdatedBy = "admin_B"
				return next, nil
			})
		assert.NoError(t, err)
	}()

	close(start)
	wg.Wait()

	// Exactly one row, and neither admin's field was lost.
	row, found, err := store.Get(bg, "spc_new")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "msg-A", row.Message, "admin A's message must not be lost on concurrent create")
	assert.True(t, row.Enabled, "admin B's enable must not be lost on concurrent create")
}
