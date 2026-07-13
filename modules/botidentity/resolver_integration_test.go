//go:build integration

package botidentity

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolverAgainstAuthoritativeBotTables(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	defer func() { _ = testutil.CleanAllTables(ctx) }()

	insertRobot := func(uid string, status int) {
		t.Helper()
		_, err := ctx.DB().InsertBySql("INSERT INTO robot(robot_id,status) VALUES(?,?)", uid, status).Exec()
		require.NoError(t, err)
	}
	insertAppBot := func(id, uid string, status int) {
		t.Helper()
		_, err := ctx.DB().InsertBySql(`
			INSERT INTO app_bot(id,uid,display_name,scope,status,token,created_by)
			VALUES(?,?,?,'platform',?,?,?)`, id, uid, uid, status, "app_token_"+id, "owner").Exec()
		require.NoError(t, err)
	}

	insertRobot("active_robot", 1)
	insertRobot("disabled_robot", 0)
	insertAppBot("published", "published_bot", 1)
	insertAppBot("draft", "draft_bot", 0)
	insertAppBot("unpublished", "unpublished_bot", 2)
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO user(uid,name,robot,status) VALUES(?,?,1,1)",
		"presentation_only", "Presentation Only",
	).Exec()
	require.NoError(t, err)
	insertRobot("ambiguous_bot", 1)
	insertAppBot("ambiguous", "ambiguous_bot", 1)

	r := New(ctx)
	tests := []struct {
		name     string
		uid      string
		wantKind Kind
		wantNil  bool
		wantErr  error
	}{
		{name: "active robot", uid: "active_robot", wantKind: KindUserBot},
		{name: "disabled robot", uid: "disabled_robot", wantNil: true},
		{name: "missing robot", uid: "missing_robot", wantNil: true},
		{name: "published app bot", uid: "published_bot", wantKind: KindAppBot},
		{name: "draft app bot", uid: "draft_bot", wantNil: true},
		{name: "unpublished app bot", uid: "unpublished_bot", wantNil: true},
		{name: "presentation metadata only", uid: "presentation_only", wantNil: true},
		{name: "ambiguous identity", uid: "ambiguous_bot", wantErr: ErrAmbiguousIdentity},
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
		})
	}
}
