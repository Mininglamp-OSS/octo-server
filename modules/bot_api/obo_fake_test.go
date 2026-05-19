// Package bot_api · YUJ-1166 — In-memory fake oboStore used across the OBO
// unit tests (checkOBO, REST handlers, fan-out). Mirrors the production
// row/cache semantics closely enough that any test that compiles against
// the oboStore interface can swap between fake and real DB without code
// changes.
//
// What the fake intentionally does NOT model:
//   - Wall-clock created_at / updated_at (returned as time.Time zero value)
//   - Cache eviction (the fake never had a cache to evict)
//   - Foreign-key cascade on grant delete (scopes survive — fine for tests)
package bot_api

import (
	"errors"
	"sync"
)

// fakeOBOStore is the in-memory oboStore used by the OBO unit tests.
// It is concurrency-safe so tests that touch it from multiple goroutines
// (e.g. fan-out spawned in a real ctx pipeline) don't race on the maps.
type fakeOBOStore struct {
	mu     sync.Mutex
	nextID int64
	grants map[int64]*oboGrantModel
	scopes map[int64]*oboScopeModel

	// Test-side error injection hooks. Defaults to nil → no error.
	failFindActiveGrant   error
	failScopeEnabled      error
	failFindGrantsChannel error
	failInsertGrant       error
	failListGrants        error
	failInsertScope       error
}

// newFakeOBOStore — constructor, zero-value-friendly so tests can also
// just `&fakeOBOStore{}` and rely on lazy init.
func newFakeOBOStore() *fakeOBOStore {
	return &fakeOBOStore{
		grants: map[int64]*oboGrantModel{},
		scopes: map[int64]*oboScopeModel{},
	}
}

func (f *fakeOBOStore) ensureInit() {
	if f.grants == nil {
		f.grants = map[int64]*oboGrantModel{}
	}
	if f.scopes == nil {
		f.scopes = map[int64]*oboScopeModel{}
	}
}

func (f *fakeOBOStore) findActiveGrantByGrantorBot(grantorUID, granteeBotUID string) (*oboGrantModel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failFindActiveGrant != nil {
		return nil, f.failFindActiveGrant
	}
	f.ensureInit()
	for _, g := range f.grants {
		if g.GrantorUID == grantorUID && g.GranteeBotUID == granteeBotUID && g.Active == 1 && g.GlobalEnabled == 1 {
			cp := *g
			return &cp, nil
		}
	}
	return nil, nil
}

func (f *fakeOBOStore) scopeEnabled(grantID int64, channelID string, channelType uint8) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failScopeEnabled != nil {
		return false, f.failScopeEnabled
	}
	f.ensureInit()
	for _, s := range f.scopes {
		if s.GrantID == grantID && s.ChannelID == channelID && s.ChannelType == channelType && s.Enabled == 1 {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeOBOStore) findActiveGrantsForChannel(channelID string, channelType uint8) ([]*oboGrantModel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failFindGrantsChannel != nil {
		return nil, f.failFindGrantsChannel
	}
	f.ensureInit()
	out := []*oboGrantModel{}
	// First collect matching grant IDs via the scopes.
	for _, s := range f.scopes {
		if s.ChannelID != channelID || s.ChannelType != channelType || s.Enabled != 1 {
			continue
		}
		g, ok := f.grants[s.GrantID]
		if !ok || g.Active != 1 || g.GlobalEnabled != 1 {
			continue
		}
		cp := *g
		out = append(out, &cp)
	}
	return out, nil
}

func (f *fakeOBOStore) insertGrant(grantorUID, granteeBotUID, mode string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failInsertGrant != nil {
		return 0, f.failInsertGrant
	}
	f.ensureInit()
	for _, g := range f.grants {
		if g.GrantorUID == grantorUID && g.GranteeBotUID == granteeBotUID {
			return 0, errors.New("Error 1062: Duplicate entry for uk_grantor_grantee")
		}
	}
	f.nextID++
	id := f.nextID
	f.grants[id] = &oboGrantModel{
		ID:            id,
		GrantorUID:    grantorUID,
		GranteeBotUID: granteeBotUID,
		Mode:          mode,
		GlobalEnabled: 0,
		Active:        1,
	}
	return id, nil
}

func (f *fakeOBOStore) listGrantsByGrantor(grantorUID string) ([]*oboGrantModel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failListGrants != nil {
		return nil, f.failListGrants
	}
	f.ensureInit()
	out := []*oboGrantModel{}
	for _, g := range f.grants {
		if g.GrantorUID == grantorUID {
			cp := *g
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (f *fakeOBOStore) findGrantByID(id int64) (*oboGrantModel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureInit()
	g, ok := f.grants[id]
	if !ok {
		return nil, nil
	}
	cp := *g
	return &cp, nil
}

func (f *fakeOBOStore) updateGrant(id int64, mode string, globalEnabled *int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureInit()
	g, ok := f.grants[id]
	if !ok {
		return nil
	}
	if mode != "" {
		g.Mode = mode
	}
	if globalEnabled != nil {
		v := 0
		if *globalEnabled != 0 {
			v = 1
		}
		g.GlobalEnabled = v
	}
	return nil
}

func (f *fakeOBOStore) revokeGrant(id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureInit()
	g, ok := f.grants[id]
	if !ok {
		return nil
	}
	g.Active = 0
	g.GlobalEnabled = 0
	return nil
}

func (f *fakeOBOStore) insertScope(grantID int64, channelID string, channelType uint8, enabled int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failInsertScope != nil {
		return 0, f.failInsertScope
	}
	f.ensureInit()
	for _, s := range f.scopes {
		if s.GrantID == grantID && s.ChannelID == channelID && s.ChannelType == channelType {
			return 0, errors.New("Error 1062: Duplicate entry for uk_grant_channel")
		}
	}
	f.nextID++
	id := f.nextID
	v := 0
	if enabled != 0 {
		v = 1
	}
	f.scopes[id] = &oboScopeModel{
		ID:          id,
		GrantID:     grantID,
		ChannelID:   channelID,
		ChannelType: channelType,
		Enabled:     v,
	}
	return id, nil
}

func (f *fakeOBOStore) deleteScope(id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureInit()
	delete(f.scopes, id)
	return nil
}

func (f *fakeOBOStore) listScopesByGrant(grantID int64) ([]*oboScopeModel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureInit()
	out := []*oboScopeModel{}
	for _, s := range f.scopes {
		if s.GrantID == grantID {
			cp := *s
			out = append(out, &cp)
		}
	}
	return out, nil
}
