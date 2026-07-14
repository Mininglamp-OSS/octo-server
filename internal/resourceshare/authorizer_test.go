package resourceshare

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/gocraft/dbr/v2"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	authActor = "user-a"
	authPeer  = "user-b"
	authSpace = "space-a"
	authGroup = "group-a"
)

func newAuthorizerHarness(t *testing.T) (*HumanTargetAuthorizer, *dbr.Session, time.Time) {
	t.Helper()
	conn, err := dbr.Open("sqlite3", ":memory:", nil)
	require.NoError(t, err)
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	session := conn.NewSession(nil)

	schema := []string{
		"CREATE TABLE space (space_id TEXT PRIMARY KEY, status INTEGER NOT NULL)",
		"CREATE TABLE space_member (space_id TEXT NOT NULL, uid TEXT NOT NULL, status INTEGER NOT NULL, PRIMARY KEY(space_id, uid))",
		"CREATE TABLE \"group\" (group_no TEXT PRIMARY KEY, space_id TEXT NOT NULL, status INTEGER NOT NULL, forbidden INTEGER NOT NULL DEFAULT 0)",
		"CREATE TABLE group_member (group_no TEXT NOT NULL, uid TEXT NOT NULL, status INTEGER NOT NULL, is_deleted INTEGER NOT NULL DEFAULT 0, role INTEGER NOT NULL DEFAULT 0, forbidden_expir_time INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(group_no, uid))",
		"CREATE TABLE thread (group_no TEXT NOT NULL, short_id TEXT NOT NULL, status INTEGER NOT NULL, PRIMARY KEY(group_no, short_id))",
	}
	for _, stmt := range schema {
		_, err := session.Exec(stmt)
		require.NoError(t, err)
	}

	_, err = session.Exec("INSERT INTO space(space_id,status) VALUES(?,1)", authSpace)
	require.NoError(t, err)
	for _, uid := range []string{authActor, authPeer} {
		_, err = session.Exec("INSERT INTO space_member(space_id,uid,status) VALUES(?,?,1)", authSpace, uid)
		require.NoError(t, err)
	}
	_, err = session.Exec("INSERT INTO \"group\"(group_no,space_id,status,forbidden) VALUES(?,?,?,0)", authGroup, authSpace, group.GroupStatusNormal)
	require.NoError(t, err)
	_, err = session.Exec("INSERT INTO group_member(group_no,uid,status,is_deleted,role,forbidden_expir_time) VALUES(?,?,?,?,?,0)",
		authGroup, authActor, int(common.GroupMemberStatusNormal), 0, int(common.GroupMemberRoleNormal))
	require.NoError(t, err)
	_, err = session.Exec("INSERT INTO thread(group_no,short_id,status) VALUES(?,?,?)", authGroup, "topic-a", thread.ThreadStatusActive)
	require.NoError(t, err)

	now := time.Unix(1_800_000_000, 0).UTC()
	authorizer := NewHumanTargetAuthorizer(session)
	authorizer.now = func() time.Time { return now }
	return authorizer, session, now
}

func TestHumanTargetAuthorizer_AllowsDMGroupAndActiveThread(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(*testing.T, *dbr.Session)
		target Target
	}{
		{"dm", nil, Target{Kind: TargetDM, PeerUID: authPeer}},
		{"group", nil, Target{Kind: TargetGroup, GroupNo: authGroup}},
		{"active thread", nil, Target{Kind: TargetThread, GroupNo: authGroup, ShortID: "topic-a"}},
		{
			name: "group forbidden allows manager",
			setup: func(t *testing.T, session *dbr.Session) {
				_, err := session.Exec("UPDATE \"group\" SET forbidden=1 WHERE group_no=?", authGroup)
				require.NoError(t, err)
				_, err = session.Exec("UPDATE group_member SET role=? WHERE group_no=? AND uid=?", int(common.GroupMemberRoleManager), authGroup, authActor)
				require.NoError(t, err)
			},
			target: Target{Kind: TargetGroup, GroupNo: authGroup},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			authorizer, session, _ := newAuthorizerHarness(t)
			if tt.setup != nil {
				tt.setup(t, session)
			}
			require.NoError(t, authorizer.Authorize(context.Background(), authActor, authSpace, tt.target))
		})
	}
}

func TestHumanTargetAuthorizer_DeniesInactiveSpaceAndMembershipFailures(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(*testing.T, *dbr.Session)
		target Target
	}{
		{"inactive space", func(t *testing.T, s *dbr.Session) {
			_, err := s.Exec("UPDATE space SET status=0 WHERE space_id=?", authSpace)
			require.NoError(t, err)
		}, Target{Kind: TargetDM, PeerUID: authPeer}},
		{"actor not active space member", func(t *testing.T, s *dbr.Session) {
			_, err := s.Exec("UPDATE space_member SET status=0 WHERE space_id=? AND uid=?", authSpace, authActor)
			require.NoError(t, err)
		}, Target{Kind: TargetDM, PeerUID: authPeer}},
		{"peer not active space member", func(t *testing.T, s *dbr.Session) {
			_, err := s.Exec("DELETE FROM space_member WHERE space_id=? AND uid=?", authSpace, authPeer)
			require.NoError(t, err)
		}, Target{Kind: TargetDM, PeerUID: authPeer}},
		{"self dm", nil, Target{Kind: TargetDM, PeerUID: authActor}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			authorizer, session, _ := newAuthorizerHarness(t)
			if tt.setup != nil {
				tt.setup(t, session)
			}
			err := authorizer.Authorize(context.Background(), authActor, authSpace, tt.target)
			assert.ErrorIs(t, err, ErrTargetDenied)
		})
	}
}

func TestHumanTargetAuthorizer_GroupDenialMatrix(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, *dbr.Session, time.Time)
	}{
		{"wrong space", func(t *testing.T, s *dbr.Session, _ time.Time) {
			_, err := s.Exec("UPDATE \"group\" SET space_id='space-b' WHERE group_no=?", authGroup)
			require.NoError(t, err)
		}},
		{"disabled group", func(t *testing.T, s *dbr.Session, _ time.Time) {
			_, err := s.Exec("UPDATE \"group\" SET status=? WHERE group_no=?", group.GroupStatusDisabled, authGroup)
			require.NoError(t, err)
		}},
		{"disbanded group", func(t *testing.T, s *dbr.Session, _ time.Time) {
			_, err := s.Exec("UPDATE \"group\" SET status=? WHERE group_no=?", group.GroupStatusDisband, authGroup)
			require.NoError(t, err)
		}},
		{"missing member", func(t *testing.T, s *dbr.Session, _ time.Time) {
			_, err := s.Exec("DELETE FROM group_member WHERE group_no=? AND uid=?", authGroup, authActor)
			require.NoError(t, err)
		}},
		{"blacklisted member", func(t *testing.T, s *dbr.Session, _ time.Time) {
			_, err := s.Exec("UPDATE group_member SET status=? WHERE group_no=? AND uid=?", int(common.GroupMemberStatusBlacklist), authGroup, authActor)
			require.NoError(t, err)
		}},
		{"deleted member", func(t *testing.T, s *dbr.Session, _ time.Time) {
			_, err := s.Exec("UPDATE group_member SET is_deleted=1 WHERE group_no=? AND uid=?", authGroup, authActor)
			require.NoError(t, err)
		}},
		{"individually muted", func(t *testing.T, s *dbr.Session, now time.Time) {
			_, err := s.Exec("UPDATE group_member SET forbidden_expir_time=? WHERE group_no=? AND uid=?", now.Add(time.Minute).Unix(), authGroup, authActor)
			require.NoError(t, err)
		}},
		{"group forbidden normal member", func(t *testing.T, s *dbr.Session, _ time.Time) {
			_, err := s.Exec("UPDATE \"group\" SET forbidden=1 WHERE group_no=?", authGroup)
			require.NoError(t, err)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			authorizer, session, now := newAuthorizerHarness(t)
			tt.setup(t, session, now)
			err := authorizer.Authorize(context.Background(), authActor, authSpace, Target{Kind: TargetGroup, GroupNo: authGroup})
			assert.ErrorIs(t, err, ErrTargetDenied)
		})
	}
}

func TestHumanTargetAuthorizer_ExpiredIndividualMuteDoesNotDeny(t *testing.T) {
	authorizer, session, now := newAuthorizerHarness(t)
	_, err := session.Exec("UPDATE group_member SET forbidden_expir_time=? WHERE group_no=? AND uid=?", now.Add(-time.Second).Unix(), authGroup, authActor)
	require.NoError(t, err)
	require.NoError(t, authorizer.Authorize(context.Background(), authActor, authSpace, Target{Kind: TargetGroup, GroupNo: authGroup}))
}

func TestHumanTargetAuthorizer_ThreadRequiresExactActiveParent(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, *dbr.Session)
	}{
		{"missing thread", func(t *testing.T, s *dbr.Session) {
			_, err := s.Exec("DELETE FROM thread WHERE group_no=?", authGroup)
			require.NoError(t, err)
		}},
		{"archived thread", func(t *testing.T, s *dbr.Session) {
			_, err := s.Exec("UPDATE thread SET status=? WHERE group_no=?", thread.ThreadStatusArchived, authGroup)
			require.NoError(t, err)
		}},
		{"deleted thread", func(t *testing.T, s *dbr.Session) {
			_, err := s.Exec("UPDATE thread SET status=? WHERE group_no=?", thread.ThreadStatusDeleted, authGroup)
			require.NoError(t, err)
		}},
		{"inactive parent", func(t *testing.T, s *dbr.Session) {
			_, err := s.Exec("UPDATE \"group\" SET status=? WHERE group_no=?", group.GroupStatusDisband, authGroup)
			require.NoError(t, err)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			authorizer, session, _ := newAuthorizerHarness(t)
			tt.setup(t, session)
			err := authorizer.Authorize(context.Background(), authActor, authSpace, Target{Kind: TargetThread, GroupNo: authGroup, ShortID: "topic-a"})
			assert.ErrorIs(t, err, ErrTargetDenied)
		})
	}
}

func TestHumanTargetAuthorizer_FailsClosedOnDBErrorAndCancellation(t *testing.T) {
	t.Run("database error", func(t *testing.T) {
		authorizer, session, _ := newAuthorizerHarness(t)
		_, err := session.Exec("DROP TABLE space")
		require.NoError(t, err)
		err = authorizer.Authorize(context.Background(), authActor, authSpace, Target{Kind: TargetDM, PeerUID: authPeer})
		assert.ErrorIs(t, err, ErrTargetQuery)
		assert.False(t, errors.Is(err, ErrTargetDenied))
	})

	t.Run("cancelled context", func(t *testing.T) {
		authorizer, _, _ := newAuthorizerHarness(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := authorizer.Authorize(ctx, authActor, authSpace, Target{Kind: TargetDM, PeerUID: authPeer})
		assert.ErrorIs(t, err, context.Canceled)
	})
}
