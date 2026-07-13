package botidentity

import (
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeActiveKindStore struct {
	record identityRecord
	err    error
	calls  int
}

func (f *fakeActiveKindStore) lookup(string) (identityRecord, error) {
	f.calls++
	return f.record, f.err
}

func TestResolverResolve(t *testing.T) {
	dbErr := errors.New("db unavailable")
	tests := []struct {
		name     string
		uid      string
		store    *fakeActiveKindStore
		wantKind Kind
		wantNil  bool
		wantErr  error
		calls    int
	}{
		{name: "empty uid", uid: "", store: &fakeActiveKindStore{}, wantNil: true, calls: 0},
		{name: "missing", uid: "missing", store: &fakeActiveKindStore{}, wantNil: true, calls: 1},
		{name: "active user bot", uid: "user_bot", store: &fakeActiveKindStore{record: identityRecord{UserBot: true, CreatorUID: "owner"}}, wantKind: KindUserBot, calls: 1},
		{name: "published app bot", uid: "app_bot", store: &fakeActiveKindStore{record: identityRecord{AppBot: true, AppScope: ScopeSpace, AppSpaceID: "space-a"}}, wantKind: KindAppBot, calls: 1},
		{name: "ambiguous active identity", uid: "both", store: &fakeActiveKindStore{record: identityRecord{UserBot: true, AppBot: true}}, wantErr: ErrAmbiguousIdentity, calls: 1},
		{name: "lookup failure", uid: "broken", store: &fakeActiveKindStore{err: dbErr}, wantErr: dbErr, calls: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Resolver{store: tt.store}
			got, err := r.Resolve(tt.uid)
			if tt.wantErr != nil {
				require.Error(t, err)
				assert.ErrorIs(t, err, tt.wantErr)
				assert.Nil(t, got)
			} else {
				require.NoError(t, err)
				if tt.wantNil {
					assert.Nil(t, got)
				} else {
					require.NotNil(t, got)
					assert.Equal(t, tt.uid, got.UID)
					assert.Equal(t, tt.wantKind, got.Kind)
					if tt.wantKind == KindUserBot {
						assert.Equal(t, "owner", got.CreatorUID)
					}
					if tt.wantKind == KindAppBot {
						assert.Equal(t, ScopeSpace, got.AppScope)
						assert.Equal(t, "space-a", got.AppSpaceID)
					}
				}
			}
			assert.Equal(t, tt.calls, tt.store.calls)
		})
	}
}

func TestResolverActivePreservesErrors(t *testing.T) {
	r := &Resolver{store: &fakeActiveKindStore{record: identityRecord{UserBot: true, AppBot: true}}}
	active, err := r.Active("both")
	assert.False(t, active)
	assert.ErrorIs(t, err, ErrAmbiguousIdentity)
}

func TestResolverActive(t *testing.T) {
	r := &Resolver{store: &fakeActiveKindStore{record: identityRecord{AppBot: true}}}
	active, err := r.Active("app")
	require.NoError(t, err)
	assert.True(t, active)
}

func TestResolverUnavailable(t *testing.T) {
	r := New(nil)
	identity, err := r.Resolve("bot")
	assert.Nil(t, identity)
	assert.ErrorIs(t, err, ErrResolverUnavailable)
}

func TestDBActiveKindStore(t *testing.T) {
	newStore := func(t *testing.T) (*dbActiveKindStore, sqlmock.Sqlmock) {
		t.Helper()
		rawDB, mock, err := sqlmock.New()
		require.NoError(t, err)
		t.Cleanup(func() { _ = rawDB.Close() })
		conn := &dbr.Connection{DB: rawDB, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}
		return &dbActiveKindStore{session: conn.NewSession(nil)}, mock
	}

	t.Run("returns both predicates", func(t *testing.T) {
		store, mock := newStore(t)
		mock.ExpectQuery(`(?s)SELECT.*EXISTS.*robot.*creator_uid.*EXISTS.*app_bot.*scope.*space_id`).
			WillReturnRows(sqlmock.NewRows([]string{"user_bot", "creator_uid", "app_bot", "app_scope", "app_space_id"}).AddRow(1, "owner-a", 0, "", ""))

		record, err := store.lookup("bot")
		require.NoError(t, err)
		assert.True(t, record.UserBot)
		assert.False(t, record.AppBot)
		assert.Equal(t, "owner-a", record.CreatorUID)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("preserves query error", func(t *testing.T) {
		store, mock := newStore(t)
		dbErr := errors.New("query failed")
		mock.ExpectQuery(`(?s)SELECT.*EXISTS.*robot.*creator_uid.*EXISTS.*app_bot.*scope.*space_id`).WillReturnError(dbErr)

		record, err := store.lookup("bot")
		assert.False(t, record.UserBot)
		assert.False(t, record.AppBot)
		assert.ErrorIs(t, err, dbErr)
		require.NoError(t, mock.ExpectationsWereMet())
	})
}
