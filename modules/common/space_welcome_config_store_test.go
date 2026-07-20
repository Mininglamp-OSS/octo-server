package common

import (
	"context"
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
