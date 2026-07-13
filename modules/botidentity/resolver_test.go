package botidentity

import (
	"errors"
	"testing"

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
