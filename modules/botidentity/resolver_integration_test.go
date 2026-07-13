//go:build integration

package botidentity

import (
	"testing"

	"github.com/gocraft/dbr/v2"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolverAgainstAuthoritativeBotTables(t *testing.T) {
	// Use an isolated real SQL database rather than the process-wide MySQL test
	// schema: resolver semantics need only the three authoritative columns, and
	// isolation avoids cross-package migration/cleanup races.
	conn, err := dbr.Open("sqlite3", ":memory:", nil)
	require.NoError(t, err)
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = conn.Close() })
	session := conn.NewSession(nil)
	_, err = session.Exec("CREATE TABLE robot (robot_id TEXT PRIMARY KEY, status INTEGER NOT NULL, creator_uid TEXT NOT NULL DEFAULT '')")
	require.NoError(t, err)
	_, err = session.Exec("CREATE TABLE app_bot (uid TEXT PRIMARY KEY, status INTEGER NOT NULL, scope TEXT NOT NULL DEFAULT 'platform', space_id TEXT)")
	require.NoError(t, err)
	_, err = session.Exec("CREATE TABLE user (uid TEXT PRIMARY KEY, robot INTEGER NOT NULL, status INTEGER NOT NULL)")
	require.NoError(t, err)

	insertRobot := func(uid string, status int) {
		t.Helper()
		_, err := session.InsertBySql("INSERT INTO robot(robot_id,status,creator_uid) VALUES(?,?,?)", uid, status, "owner-"+uid).Exec()
		require.NoError(t, err)
	}
	insertAppBot := func(uid string, status int) {
		t.Helper()
		_, err := session.InsertBySql("INSERT INTO app_bot(uid,status,scope,space_id) VALUES(?,?,?,?)", uid, status, "space", "space-a").Exec()
		require.NoError(t, err)
	}

	insertRobot("identity_test_active_robot", 1)
	insertRobot("identity_test_disabled_robot", 0)
	insertAppBot("identity_test_published_bot", 1)
	insertAppBot("identity_test_draft_bot", 0)
	insertAppBot("identity_test_unpublished_bot", 2)
	_, err = session.InsertBySql(
		"INSERT INTO user(uid,robot,status) VALUES(?,1,1)",
		"identity_test_presentation_only",
	).Exec()
	require.NoError(t, err)
	insertRobot("identity_test_ambiguous_bot", 1)
	insertAppBot("identity_test_ambiguous_bot", 1)

	r := &Resolver{store: &dbActiveKindStore{session: session}}
	tests := []struct {
		name     string
		uid      string
		wantKind Kind
		wantNil  bool
		wantErr  error
	}{
		{name: "active robot", uid: "identity_test_active_robot", wantKind: KindUserBot},
		{name: "disabled robot", uid: "identity_test_disabled_robot", wantNil: true},
		{name: "missing robot", uid: "identity_test_missing_robot", wantNil: true},
		{name: "published app bot", uid: "identity_test_published_bot", wantKind: KindAppBot},
		{name: "draft app bot", uid: "identity_test_draft_bot", wantNil: true},
		{name: "unpublished app bot", uid: "identity_test_unpublished_bot", wantNil: true},
		{name: "presentation metadata only", uid: "identity_test_presentation_only", wantNil: true},
		{name: "ambiguous identity", uid: "identity_test_ambiguous_bot", wantErr: ErrAmbiguousIdentity},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := r.Resolve(tt.uid)
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			if tt.wantNil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, tt.uid, got.UID)
			assert.Equal(t, tt.wantKind, got.Kind)
			if tt.wantKind == KindUserBot {
				assert.Equal(t, "owner-"+tt.uid, got.CreatorUID)
			}
			if tt.wantKind == KindAppBot {
				assert.Equal(t, ScopeSpace, got.AppScope)
				assert.Equal(t, "space-a", got.AppSpaceID)
			}
		})
	}
}
