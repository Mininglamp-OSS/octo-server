package aiteam

import (
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
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

func TestProtectedPurposes(t *testing.T) {
	assert.True(t, IsProtectedPurpose(GroupPurpose))
	assert.True(t, IsProtectedPurpose(TeamGroupPurpose))
	assert.True(t, IsProtectedPurpose(CustomTeamPurpose))
	assert.False(t, IsProtectedPurpose(""))
	assert.False(t, IsProtectedPurpose("ordinary"))
	assert.True(t, IsHiddenPurpose(GroupPurpose))
	assert.True(t, IsHiddenPurpose(TeamGroupPurpose))
	assert.True(t, IsHiddenPurpose(CustomTeamPurpose))
	assert.True(t, IsImmutablePurpose(GroupPurpose))
	assert.True(t, IsImmutablePurpose(TeamGroupPurpose))
	assert.False(t, IsImmutablePurpose(CustomTeamPurpose))
	assert.True(t, IsDedicatedSessionPurpose(GroupPurpose))
	assert.False(t, IsDedicatedSessionPurpose(TeamGroupPurpose))
}

func TestExcludeProtectedItemsSkipsNonGroupChannelsBeforeDB(t *testing.T) {
	protected, err := ExcludeProtectedItems(nil, [][2]string{
		{"person-a", "1"},
		{"person-b", "1"},
	})
	require.NoError(t, err)
	assert.Empty(t, protected)
}

func TestMaybeSetDefaultSessionTitle(t *testing.T) {
	rawDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = rawDB.Close() })
	session := (&dbr.Connection{
		DB:            rawDB,
		EventReceiver: &dbr.NullEventReceiver{},
		Dialect:       dialect.MySQL,
	}).NewSession(nil)

	longTitle := strings.Repeat("会", 101)
	mock.ExpectExec(`(?s)UPDATE thread t.*JOIN ai_team_session s.*JOIN ai_team_agent a.*SET t.name=.*WHERE t.group_no=.*t.short_id=.*t.name=`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	require.NoError(t, MaybeSetDefaultSessionTitle(session, "group", "session", "owner", "  "+longTitle+"  "))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMaybeSetDefaultSessionTitleSkipsIncompleteInput(t *testing.T) {
	require.NoError(t, MaybeSetDefaultSessionTitle(nil, "group", "session", "owner", "hello"))
	require.NoError(t, MaybeSetDefaultSessionTitle(&dbr.Session{}, "", "session", "owner", "hello"))
	require.NoError(t, MaybeSetDefaultSessionTitle(&dbr.Session{}, "group", "session", "owner", "  "))
}
