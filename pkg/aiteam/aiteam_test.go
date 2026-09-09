package aiteam

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnabled(t *testing.T) {
	for _, value := range []string{"1", "true", " TRUE "} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("DM_AI_TEAM_ON", value)
			assert.True(t, Enabled())
		})
	}
	for _, value := range []string{"", "0", "false", "invalid"} {
		t.Run("off_"+value, func(t *testing.T) {
			t.Setenv("DM_AI_TEAM_ON", value)
			assert.False(t, Enabled())
		})
	}
}

func TestExcludeProtectedItemsSkipsNonGroupChannelsBeforeDB(t *testing.T) {
	protected, err := ExcludeProtectedItems(nil, [][2]string{
		{"person-a", "1"},
		{"person-b", "1"},
	})
	require.NoError(t, err)
	assert.Empty(t, protected)
}
