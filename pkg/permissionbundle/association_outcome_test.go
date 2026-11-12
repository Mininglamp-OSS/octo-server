package permissionbundle

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/require"
)

func TestAssociationAcknowledgementFailurePreservesCommittedScope(t *testing.T) {
	s := fixtureStore(t, filepath.Join(t.TempDir(), "ack.sqlite"))
	seed(t, s)
	_, e := s.Session.Exec(`ALTER TABLE robot ADD COLUMN bot_token TEXT DEFAULT 'bf_synthetic'`)
	require.NoError(t, e)
	s.Authority = fixtureAuthority{ackFails: true}
	c := &config.Context{}
	require.NoError(t, Install(c, s, fixturePin, nil))
	tx, cancel, e := BeginAssociation(c, s.Session)
	require.NoError(t, e)
	defer cancel()
	_, e = tx.Exec(`INSERT INTO user VALUES(5,'bot2',1,0,1)`)
	require.NoError(t, e)
	_, e = tx.Exec(`INSERT INTO robot(id,robot_id,creator_uid,status) VALUES(5,'bot2','owner',1)`)
	require.NoError(t, e)
	_, e = tx.Exec(`INSERT INTO space_member VALUES('space','bot2',1)`)
	require.NoError(t, e)
	e = CommitAssociations(c, tx, Association{BotUID: "bot2", SpaceID: "space", New: true})
	require.ErrorIs(t, e, ErrReconcile)
	require.ErrorIs(t, e, ErrUnavailable)
	tx.RollbackUnlessCommitted() // cannot undo the durable native/header pair
	s.Authority = fixtureAuthority{}
	key := fixtureKey
	key.BotUID = "bot2"
	after, e := s.Get(t.Context(), key, "owner")
	require.NoError(t, e)
	require.Equal(t, "0", after.Bundle.Revision)
	require.NotEmpty(t, after.Bundle.Incarnation)
	var count int
	require.NoError(t, s.Session.QueryRow(`SELECT count(*) FROM space_member WHERE uid='bot2'`).Scan(&count))
	require.Equal(t, 1, count)
}

type contextCheckingAuthority struct {
	t        *testing.T
	deadline time.Time
	key      any
}

func (f contextCheckingAuthority) Begin(ctx context.Context, _ *dbr.Tx) (AuthorityRead, error) {
	deadline, ok := ctx.Deadline()
	require.True(f.t, ok)
	require.Equal(f.t, f.deadline, deadline)
	require.Equal(f.t, "request", ctx.Value(f.key))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return fixtureAuthority{}, nil
}

func TestAssociationSuffixRetainsOriginalContextAndDeadline(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "cancelled"}[cancelled], func(t *testing.T) {
			s := fixtureStore(t, filepath.Join(t.TempDir(), "context.sqlite"))
			seed(t, s)
			key := &struct{ name string }{"request"}
			parent, cancelParent := context.WithTimeout(context.WithValue(t.Context(), key, "request"), time.Second)
			defer cancelParent()
			deadline, _ := parent.Deadline()
			s.Authority = contextCheckingAuthority{t, deadline, key}
			c := &config.Context{}
			require.NoError(t, Install(c, s, fixturePin, nil))
			tx, cancel, e := BeginAssociationContext(parent, c, s.Session)
			require.NoError(t, e)
			defer cancel()
			defer tx.RollbackUnlessCommitted()
			if cancelled {
				cancelParent()
			}
			e = CommitAssociations(c, tx)
			if cancelled {
				require.ErrorIs(t, e, ErrUnavailable)
			} else {
				require.NoError(t, e)
			}
		})
	}
}
