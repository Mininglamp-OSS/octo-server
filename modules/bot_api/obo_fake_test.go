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
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
)

// fakeOBOStore is the in-memory oboStore used by the OBO unit tests.
// It is concurrency-safe so tests that touch it from multiple goroutines
// (e.g. fan-out spawned in a real ctx pipeline) don't race on the maps.
type fakeOBOStore struct {
	mu     sync.Mutex
	nextID int64
	grants map[int64]*oboGrantModel
	scopes map[int64]*oboScopeModel
	// robotOwners maps botUID → CreatorUID. A row in this map means
	// "registered as a bot" (IsBot=true). Used by queryRobotOwner. Tests
	// that exercise oboCreateGrant's owner-check must seed this map.
	robotOwners map[string]string
	// nonBotUsers — uids that exist in the user table but are NOT bots
	// (user.robot=0). queryRobotOwner returns IsBot=false for these. Used
	// to test the "grantee_bot_uid is a real user, not a bot" rejection.
	nonBotUsers map[string]bool
	// botNames maps botUID → display name (user.name). listGrantsByGrantor
	// reads this map to populate GranteeBotName, mirroring the prod LEFT
	// JOIN against the `user` table (YUJ-1358). When a name is absent the
	// fake falls back to the bot uid (same as prod's COALESCE).
	botNames map[string]string
	// groupMembers maps groupNo → set of uids currently in the group
	// (`group_member.is_deleted=0`). Used by findGlobalGrantsWithoutScope
	// to mirror the prod SQL JOIN that filters implicit-scope candidates
	// down to "grantor IS member AND bot IS NOT member" (PR#121). Tests
	// that exercise the implicit-scope fan-out path seed this map via
	// seedGroupMember.
	groupMembers map[string]map[string]bool

	// Test-side error injection hooks. Defaults to nil → no error.
	failFindActiveGrant   error
	failScopeEnabled      error
	failScopeRowExists    error
	failFindGrantsChannel error
	failInsertGrant       error
	failListGrants        error
	failInsertScope       error
	failQueryRobotOwner   error
	failFindScopeOwner    error

	// PR#114 R3 (Jerry-Xin perf blocker) — call counters so tests can
	// pin the early-return contract: on plain / @AI-only group traffic,
	// neither findActiveGrantsForChannel nor
	// findActiveGrantsForChannelByGrantors must be invoked. Mirrors the
	// production negative-cache short-circuit: the cheapest grant
	// lookup is the one we never make.
	findGrantsChannelCalls           int
	findGrantsChannelByGrantorsCalls int
	// lastFindByGrantorsArgs records the most recent argument set passed
	// to findActiveGrantsForChannelByGrantors so tests can assert that
	// the @grantor narrowing actually filtered the query (and didn't
	// silently fall back to the unfiltered scan).
	lastFindByGrantorsArgs struct {
		channelID   string
		channelType uint8
		grantorUIDs []string
		called      bool
	}
}

// newFakeOBOStore — constructor, zero-value-friendly so tests can also
// just `&fakeOBOStore{}` and rely on lazy init.
func newFakeOBOStore() *fakeOBOStore {
	return &fakeOBOStore{
		grants:       map[int64]*oboGrantModel{},
		scopes:       map[int64]*oboScopeModel{},
		robotOwners:  map[string]string{},
		nonBotUsers:  map[string]bool{},
		botNames:     map[string]string{},
		groupMembers: map[string]map[string]bool{},
	}
}

func (f *fakeOBOStore) ensureInit() {
	if f.grants == nil {
		f.grants = map[int64]*oboGrantModel{}
	}
	if f.scopes == nil {
		f.scopes = map[int64]*oboScopeModel{}
	}
	if f.robotOwners == nil {
		f.robotOwners = map[string]string{}
	}
	if f.nonBotUsers == nil {
		f.nonBotUsers = map[string]bool{}
	}
	if f.botNames == nil {
		f.botNames = map[string]string{}
	}
	if f.groupMembers == nil {
		f.groupMembers = map[string]map[string]bool{}
	}
}

// seedBot registers `botUID` as a bot owned by `creatorUID`. Helper for
// tests that exercise oboCreateGrant's ownership + IsBot check.
func (f *fakeOBOStore) seedBot(botUID, creatorUID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureInit()
	f.robotOwners[botUID] = creatorUID
}

// seedBotName registers `botUID` → `displayName` for the fake's
// listGrantsByGrantor name lookup. Tests that need to assert on the
// JOIN-derived `grantee_bot_name` field should seed both ownership
// (seedBot) and a name. Unsealed bots fall back to the bot uid, mirroring
// the COALESCE in the production query (YUJ-1358).
func (f *fakeOBOStore) seedBotName(botUID, displayName string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureInit()
	f.botNames[botUID] = displayName
}

// seedNonBotUser marks `uid` as a real (human) user — exists in `user`
// table but with robot=0. queryRobotOwner returns IsBot=false / found=false
// for these (mirrors prod: queryRobotOwner only finds rows in the robot
// table). Used to test the "you can't grant OBO to a non-bot uid" path.
func (f *fakeOBOStore) seedNonBotUser(uid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureInit()
	f.nonBotUsers[uid] = true
}

// seedGroupMember marks `uid` as an active (is_deleted=0) member of
// `groupNo`. Mirrors the (group_no, uid) covering index on the real
// `group_member` table. Used by findGlobalGrantsWithoutScope to drive
// the implicit-scope JOIN filter — tests that exercise the implicit-scope
// fan-out path must seed both the grantor (so the INNER JOIN matches) and
// abstain from seeding the bot (so the LEFT JOIN's `gm_bot.uid IS NULL`
// holds). Calling this for a (groupNo, uid) that is already a member is
// a no-op.
func (f *fakeOBOStore) seedGroupMember(groupNo, uid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureInit()
	members, ok := f.groupMembers[groupNo]
	if !ok {
		members = map[string]bool{}
		f.groupMembers[groupNo] = members
	}
	members[uid] = true
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

func (f *fakeOBOStore) scopeRowExists(grantID int64, channelID string, channelType uint8) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failScopeRowExists != nil {
		return false, f.failScopeRowExists
	}
	f.ensureInit()
	for _, s := range f.scopes {
		if s.GrantID == grantID && s.ChannelID == channelID && s.ChannelType == channelType {
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

func (f *fakeOBOStore) findGlobalGrantsWithoutScope(channelID string, channelType uint8) ([]*oboGrantModel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureInit()
	out := []*oboGrantModel{}
	// Mirror the prod SQL JOIN (PR#121): implicit-scope candidates only
	// fire for GROUP channels, and a candidate must satisfy ALL of:
	//   1. active=1 AND global_enabled=1
	//   2. no obo_scopes row for (grant_id, channel_id, channel_type)
	//   3. grantor IS a member of the group (group_member, is_deleted=0)
	//   4. grantee bot is NOT a member of the group (Gate 4)
	if channelType != common.ChannelTypeGroup.Uint8() {
		return out, nil
	}
	members := f.groupMembers[channelID] // nil-map reads return zero-value
	for _, g := range f.grants {
		if g.Active != 1 || g.GlobalEnabled != 1 {
			continue
		}
		// (3) grantor must be a current member.
		if !members[g.GrantorUID] {
			continue
		}
		// (4) bot must NOT be a current member.
		if members[g.GranteeBotUID] {
			continue
		}
		// (2) no scope row for this channel — ANY scope row (regardless
		// of enabled) blocks implicit-scope, matching prod semantics
		// where an explicitly-disabled scope is an admin's intentional
		// channel exclusion.
		hasScope := false
		for _, s := range f.scopes {
			if s.GrantID == g.ID && s.ChannelID == channelID && s.ChannelType == channelType {
				hasScope = true
				break
			}
		}
		if hasScope {
			continue
		}
		cp := *g
		out = append(out, &cp)
	}
	return out, nil
}

func (f *fakeOBOStore) insertGrant(grantorUID, granteeBotUID, mode, personaPrompt string) (int64, error) {
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
		PersonaPrompt: personaPrompt,
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
			// Mirror prod's LEFT JOIN COALESCE(u.name, g.grantee_bot_uid):
			// always populate a non-empty display name (YUJ-1358).
			if name, ok := f.botNames[g.GranteeBotUID]; ok && name != "" {
				cp.GranteeBotName = name
			} else {
				cp.GranteeBotName = g.GranteeBotUID
			}
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

func (f *fakeOBOStore) updateGrant(id int64, mode string, globalEnabled *int, personaPrompt *string) error {
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
	if personaPrompt != nil {
		g.PersonaPrompt = *personaPrompt
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
	now := time.Now()
	g.RevokedAt = &now
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

// findGrantByGrantorBot — any state (active OR revoked). Mirrors prod
// signature; used by oboCreateGrant reactivation.
func (f *fakeOBOStore) findGrantByGrantorBot(grantorUID, granteeBotUID string) (*oboGrantModel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureInit()
	for _, g := range f.grants {
		if g.GrantorUID == grantorUID && g.GranteeBotUID == granteeBotUID {
			cp := *g
			return &cp, nil
		}
	}
	return nil, nil
}

// reactivateGrant — flip soft-deleted row back to insertGrant defaults.
func (f *fakeOBOStore) reactivateGrant(id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureInit()
	g, ok := f.grants[id]
	if !ok {
		return nil
	}
	g.Active = 1
	g.GlobalEnabled = 0
	g.RevokedAt = nil
	return nil
}

// findScopeOwner — O(1) lookup in the fake; mirrors prod JOIN result.
func (f *fakeOBOStore) findScopeOwner(scopeID int64) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failFindScopeOwner != nil {
		return "", false, f.failFindScopeOwner
	}
	f.ensureInit()
	s, ok := f.scopes[scopeID]
	if !ok {
		return "", false, nil
	}
	g, ok := f.grants[s.GrantID]
	if !ok {
		return "", false, nil
	}
	return g.GrantorUID, true, nil
}

// queryRobotOwner — returns creator + IsBot=true for seeded bots,
// (_, false, false, nil) for seeded non-bot users, and (_,_,false,nil)
// otherwise. Tests seed via seedBot / seedNonBotUser helpers above.
func (f *fakeOBOStore) queryRobotOwner(botUID string) (string, bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failQueryRobotOwner != nil {
		return "", false, false, f.failQueryRobotOwner
	}
	f.ensureInit()
	if creator, ok := f.robotOwners[botUID]; ok {
		return creator, true, true, nil
	}
	if f.nonBotUsers[botUID] {
		// Exists as a real user, but robot=0. Prod's queryRobotOwner only
		// reads the robot table; a non-bot user has no row there, so we
		// return found=false to match. The test-facing distinction is in
		// seedNonBotUser, which exists so future tests can distinguish
		// "uid unknown" vs "uid known but not a bot" if needed.
		return "", false, false, nil
	}
	return "", false, false, nil
}
