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
	userBot bool
	appBot  bool
	err     error
	calls   int
}

func (f *fakeActiveKindStore) activeKinds(string) (bool, bool, error) {
	f.calls++
	return f.userBot, f.appBot, f.err
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
		{name: "active user bot", uid: "user_bot", store: &fakeActiveKindStore{userBot: true}, wantKind: KindUserBot, calls: 1},
		{name: "published app bot", uid: "app_bot", store: &fakeActiveKindStore{appBot: true}, wantKind: KindAppBot, calls: 1},
		{name: "ambiguous active identity", uid: "both", store: &fakeActiveKindStore{userBot: true, appBot: true}, wantErr: ErrAmbiguousIdentity, calls: 1},
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
				}
			}
			assert.Equal(t, tt.calls, tt.store.calls)
		})
	}
}

func TestResolverActivePreservesErrors(t *testing.T) {
	r := &Resolver{store: &fakeActiveKindStore{userBot: true, appBot: true}}
	active, err := r.Active("both")
	assert.False(t, active)
	assert.ErrorIs(t, err, ErrAmbiguousIdentity)
}

func TestResolverActive(t *testing.T) {
	r := &Resolver{store: &fakeActiveKindStore{appBot: true}}
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
		mock.ExpectQuery(`(?s)SELECT.*EXISTS.*robot.*EXISTS.*app_bot`).
			WillReturnRows(sqlmock.NewRows([]string{"user_bot", "app_bot"}).AddRow(1, 0))

		userBot, appBot, err := store.activeKinds("bot")
		require.NoError(t, err)
		assert.True(t, userBot)
		assert.False(t, appBot)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("preserves query error", func(t *testing.T) {
		store, mock := newStore(t)
		dbErr := errors.New("query failed")
		mock.ExpectQuery(`(?s)SELECT.*EXISTS.*robot.*EXISTS.*app_bot`).WillReturnError(dbErr)

		userBot, appBot, err := store.activeKinds("bot")
		assert.False(t, userBot)
		assert.False(t, appBot)
		assert.ErrorIs(t, err, dbErr)
		require.NoError(t, mock.ExpectationsWereMet())
	})
}
