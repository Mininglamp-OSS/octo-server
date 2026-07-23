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

// TestValidateGroupWelcomeCombination pins the pure validator (no DB).
func TestValidateGroupWelcomeCombination(t *testing.T) {
	af := time.Now().UTC().Format(time.RFC3339)
	cases := []struct {
		name string
		cfg  GroupWelcomeConfig
		want string
	}{
		{"disabled always ok", GroupWelcomeConfig{Enabled: false}, ""},
		{"valid", GroupWelcomeConfig{Enabled: true, GroupNo: "g1", ActiveFromRaw: af, Message: "hi"}, ""},
		{"missing group_no", GroupWelcomeConfig{Enabled: true, GroupNo: "", ActiveFromRaw: af, Message: "hi"}, "group_no"},
		{"bad active_from", GroupWelcomeConfig{Enabled: true, GroupNo: "g1", ActiveFromRaw: "nope", Message: "hi"}, "active_from"},
		{"empty message", GroupWelcomeConfig{Enabled: true, GroupNo: "g1", ActiveFromRaw: af, Message: "   "}, "message"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ValidateGroupWelcomeCombination(tc.cfg))
		})
	}
}

// TestGroupWelcomeConfigStore_CRUD exercises the per-group config store end to end.
func TestGroupWelcomeConfigStore_CRUD(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })

	store := NewGroupWelcomeConfigStore(ctx.DB())
	bg := context.Background()
	now := time.Now().UTC()

	_, found, err := store.Get(bg, "g_1")
	require.NoError(t, err)
	assert.False(t, found)

	require.NoError(t, store.Upsert(bg, GroupWelcomeConfig{
		GroupNo: "g_1", Enabled: true, ActiveFromRaw: "2026-07-10T00:00:00Z", Message: "欢迎 {member}", UpdatedBy: "admin",
	}, now))

	row, found, err := store.Get(bg, "g_1")
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, row.Enabled)
	assert.Equal(t, "欢迎 {member}", row.Message)
	assert.Equal(t, "admin", row.UpdatedBy)

	// Update.
	require.NoError(t, store.Upsert(bg, GroupWelcomeConfig{
		GroupNo: "g_1", Enabled: false, ActiveFromRaw: "", Message: "updated", UpdatedBy: "admin2",
	}, now.Add(time.Minute)))
	row, _, err = store.Get(bg, "g_1")
	require.NoError(t, err)
	assert.False(t, row.Enabled)
	assert.Equal(t, "updated", row.Message)

	// ListEnabled returns only enabled rows.
	require.NoError(t, store.Upsert(bg, GroupWelcomeConfig{
		GroupNo: "g_2", Enabled: true, ActiveFromRaw: "2026-07-11T00:00:00Z", Message: "hi2", UpdatedBy: "a",
	}, now))
	list, err := store.ListEnabled(bg)
	require.NoError(t, err)
	got := map[string]bool{}
	for _, c := range list {
		got[c.GroupNo] = true
	}
	assert.True(t, got["g_2"], "enabled g_2 listed")
	assert.False(t, got["g_1"], "disabled g_1 not listed")

	// Delete is idempotent.
	deleted, err := store.Delete(bg, "g_1")
	require.NoError(t, err)
	assert.True(t, deleted)
	deleted, err = store.Delete(bg, "g_1")
	require.NoError(t, err)
	assert.False(t, deleted)
}

// TestGroupWelcomeConfigStore_UpsertMerged_NoLostUpdate: two admins patch DIFFERENT
// fields of the same existing group config at once; the row lock serializes them
// so BOTH patches survive.
func TestGroupWelcomeConfigStore_UpsertMerged_NoLostUpdate(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })

	store := NewGroupWelcomeConfigStore(ctx.DB())
	bg := context.Background()
	require.NoError(t, store.Upsert(bg, GroupWelcomeConfig{
		GroupNo: "g_race", Enabled: true, ActiveFromRaw: "2026-07-10T00:00:00Z", Message: "orig", UpdatedBy: "seed",
	}, time.Now().UTC()))

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, err := store.UpsertMerged(bg, "g_race", time.Now().UTC(),
			func(cur *GroupWelcomeConfig, found bool) (GroupWelcomeConfig, error) {
				next := GroupWelcomeConfig{GroupNo: "g_race"}
				if cur != nil {
					next = *cur
				}
				next.Message = "msg-A"
				return next, nil
			})
		assert.NoError(t, err)
	}()
	go func() {
		defer wg.Done()
		<-start
		_, err := store.UpsertMerged(bg, "g_race", time.Now().UTC(),
			func(cur *GroupWelcomeConfig, found bool) (GroupWelcomeConfig, error) {
				next := GroupWelcomeConfig{GroupNo: "g_race"}
				if cur != nil {
					next = *cur
				}
				next.Enabled = false
				return next, nil
			})
		assert.NoError(t, err)
	}()
	close(start)
	wg.Wait()

	row, found, err := store.Get(bg, "g_race")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "msg-A", row.Message, "admin A's message must not be lost")
	assert.False(t, row.Enabled, "admin B's disable must not be lost")
}

// TestGroupWelcomeConfigStore_UpsertMerged_ConcurrentCreate: two admins issue the
// FIRST PUT for the same absent config at once; insert-then-lock serializes them
// so both disjoint fields survive with exactly one row and no error.
func TestGroupWelcomeConfigStore_UpsertMerged_ConcurrentCreate(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })

	store := NewGroupWelcomeConfigStore(ctx.DB())
	bg := context.Background()

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, err := store.UpsertMerged(bg, "g_new", time.Now().UTC(),
			func(cur *GroupWelcomeConfig, found bool) (GroupWelcomeConfig, error) {
				next := GroupWelcomeConfig{GroupNo: "g_new"}
				if cur != nil {
					next = *cur
				}
				next.Message = "msg-A"
				return next, nil
			})
		assert.NoError(t, err)
	}()
	go func() {
		defer wg.Done()
		<-start
		_, err := store.UpsertMerged(bg, "g_new", time.Now().UTC(),
			func(cur *GroupWelcomeConfig, found bool) (GroupWelcomeConfig, error) {
				next := GroupWelcomeConfig{GroupNo: "g_new"}
				if cur != nil {
					next = *cur
				}
				next.Enabled = true
				return next, nil
			})
		assert.NoError(t, err)
	}()
	close(start)
	wg.Wait()

	row, found, err := store.Get(bg, "g_new")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "msg-A", row.Message, "message not lost on concurrent create")
	assert.True(t, row.Enabled, "enable not lost on concurrent create")
}
