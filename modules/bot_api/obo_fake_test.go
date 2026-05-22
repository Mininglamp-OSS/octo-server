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
	"sort"
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

// findGrantByGrantorBotActiveOnly — YUJ-1428 / PR#121 R5 / B3. Mirrors
// findActiveGrantByGrantorBot but skips the GlobalEnabled gate so the
// grantor-reply bypass keeps working when the user has toggled the
// persona's global switch off. Shares the failFindActiveGrant injection
// hook because the underlying DB error class is identical (any test that
// wants this method to fail would also want the strict variant to fail
// the same way) and tests that need to differentiate the two return
// values can do so via the GlobalEnabled flag on the seeded grant.
func (f *fakeOBOStore) findGrantByGrantorBotActiveOnly(grantorUID, granteeBotUID string) (*oboGrantModel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failFindActiveGrant != nil {
		return nil, f.failFindActiveGrant
	}
	f.ensureInit()
	for _, g := range f.grants {
		if g.GrantorUID == grantorUID && g.GranteeBotUID == granteeBotUID && g.Active == 1 {
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
	f.findGrantsChannelCalls++
	if f.failFindGrantsChannel != nil {
		return nil, f.failFindGrantsChannel
	}
	f.ensureInit()
	out := []*oboGrantModel{}
	// YUJ-1538 — mirror the production channel-type-aware lookup:
	// Group returns every active+global_enabled grant without
	// requiring a scope row (the prod fan-out path combines this
	// with a separate findGlobalGrantsWithoutScope call; aggregating
	// here keeps the fake's behavior end-to-end equivalent for
	// fanoutForMessage tests). DM (Person) keeps the strict scope-
	// row contract.
	//
	// PR#121 R6 / B3 (Jerry-Xin + lml2468 2026-05-22 blocking):
	// CommunityTopic does NOT take the implicit-global branch here.
	// Production's findActiveGrantsForChannel uses an INNER JOIN on
	// obo_scopes for ALL channel types (DM, Group, CommunityTopic),
	// and the implicit-scope feeder
	// (findGlobalGrantsWithoutScope) is only invoked from
	// fanoutForMessage when the channel type is exactly
	// ChannelTypeGroup (obo_fanout.go gate). The R5 fake treated
	// CommunityTopic the same as Group via isGroupLikeChannelType,
	// which created fake/prod divergence: tests that exercised topic
	// fan-out without seeding a scope row would pass against the
	// fake but fail in production. Aligning the fake to the prod
	// contract (CommunityTopic requires a scope row) closes the
	// divergence without expanding the production code surface.
	// CommunityTopic implicit-scope support is NOT currently
	// planned; if that changes, both production
	// (fanoutForMessage / findGlobalGrantsWithoutScope) and this
	// fake must be updated together.
	if channelType == common.ChannelTypeGroup.Uint8() {
		// Iterate by sorted grant ID so tests get deterministic ordering
		// independent of map iteration order.
		ids := make([]int64, 0, len(f.grants))
		for id := range f.grants {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for _, id := range ids {
			g := f.grants[id]
			if g == nil || g.Active != 1 || g.GlobalEnabled != 1 {
				continue
			}
			cp := *g
			out = append(out, &cp)
		}
		return out, nil
	}
	// DM path — original behavior: only grants with a matching enabled
	// scope row are surfaced.
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

// findActiveGrantsForChannelByGrantors — PR#114 R3 (Jerry-Xin perf
// blocker) fake impl. Mirrors the production `grantor_uid IN (...)`
// filter at the in-memory level so unit tests can pin both the
// behavior (right rows returned) and the call shape (was it invoked,
// with what filter set). DM / non-group-like calls return empty
// without consulting the maps, mirroring the production guard.
func (f *fakeOBOStore) findActiveGrantsForChannelByGrantors(channelID string, channelType uint8, grantorUIDs []string) ([]*oboGrantModel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.findGrantsChannelByGrantorsCalls++
	// Record the latest call so tests can assert the filter shape.
	f.lastFindByGrantorsArgs.called = true
	f.lastFindByGrantorsArgs.channelID = channelID
	f.lastFindByGrantorsArgs.channelType = channelType
	// Copy slice to insulate test assertions from caller mutations.
	f.lastFindByGrantorsArgs.grantorUIDs = append([]string(nil), grantorUIDs...)
	if f.failFindGrantsChannel != nil {
		return nil, f.failFindGrantsChannel
	}
	f.ensureInit()
	out := []*oboGrantModel{}
	if channelID == "" || len(grantorUIDs) == 0 {
		return out, nil
	}
	if !isGroupLikeChannelType(channelType) {
		return out, nil
	}
	// Build the set so membership tests are O(1).
	wanted := make(map[string]struct{}, len(grantorUIDs))
	for _, u := range grantorUIDs {
		wanted[u] = struct{}{}
	}
	// Iterate by sorted grant ID for deterministic ordering — mirrors
	// the sort applied in findActiveGrantsForChannel's group branch.
	ids := make([]int64, 0, len(f.grants))
	for id := range f.grants {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		g := f.grants[id]
		if g == nil || g.Active != 1 || g.GlobalEnabled != 1 {
			continue
		}
		if _, ok := wanted[g.GrantorUID]; !ok {
			continue
		}
		// PR#121 R5 / B1 — explicit `enabled=0` scope row for this
		// (grant, channel, channel_type) suppresses the @-mention
		// fan-out, mirroring the LEFT JOIN anti-join in the prod
		// SQL. Allow rows (enabled=1) and absence-of-row both keep
		// the grant eligible.
		disabled := false
		for _, s := range f.scopes {
			if s == nil {
				continue
			}
			if s.GrantID == g.ID && s.ChannelID == channelID &&
				s.ChannelType == channelType && s.Enabled == 0 {
				disabled = true
				break
			}
		}
		if disabled {
			continue
		}
		cp := *g
		out = append(out, &cp)
	}
	return out, nil
}

func (f *fakeOBOStore) findGlobalGrantsWithoutScope(membershipGroupID, channelID string, channelType uint8) ([]*oboGrantModel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureInit()
	out := []*oboGrantModel{}
	// Mirror the prod SQL JOIN (PR#121): implicit-scope candidates only
	// fire for group-like channels (Group / CommunityTopic per PR#121 R9 /
	// YUJ-1676), and a candidate must satisfy ALL of:
	//   1. active=1 AND global_enabled=1
	//   2. no obo_scopes row for (grant_id, channel_id, channel_type)
	//   3. grantor IS a member of `membershipGroupID` (group_member,
	//      is_deleted=0); for CommunityTopic that's the parent group, for
	//      Group it's the channel itself.
	//   4. grantee bot is NOT a member of `membershipGroupID` (Gate 4)
	if !isGroupLikeChannelType(channelType) {
		return out, nil
	}
	if membershipGroupID == "" || channelID == "" {
		return out, nil
	}
	members := f.groupMembers[membershipGroupID] // nil-map reads return zero-value
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

// setGrantActive — YUJ-1728 / octo-server#129. In-memory analogue of
// the prod tx; the outer mu serializes all writes so the activate
// path's mutex semantics fall out naturally without an explicit lock
// dance. Mirrors the prod implementation's two paths — pause is a
// single-row mutation, activate flips the target then demotes every
// other active grant under the same grantor. Demotion sets
// active=0 / global_enabled=0 / revoked_at=now so the in-memory state
// matches what createOrReactivateGrantAtomic also produces.
//
// YUJ-1738 / PR#131 R2 B2 — race guard parity: both paths short-
// circuit when the loaded row already carries revoked_at != nil.
// Without this, a test that interleaves revokeGrant + setGrantActive
// would observe in-memory behavior diverging from prod (prod refuses
// to resurrect; the fake would silently flip active back to 1).
func (f *fakeOBOStore) setGrantActive(id int64, active int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureInit()
	g, ok := f.grants[id]
	if !ok {
		return nil
	}
	// YUJ-1738 / PR#131 R2 B2 — refuse to mutate a tombstoned row
	// in either direction. Pause on a revoked row is a no-op (it is
	// already active=0); activate on a revoked row would resurrect
	// it, which is exactly the DELETE-vs-PUT race the prod code now
	// guards against in-tx.
	if g.RevokedAt != nil {
		return nil
	}
	v := 0
	if active != 0 {
		v = 1
	}
	if v == 0 {
		g.Active = 0
		return nil
	}
	g.Active = 1
	g.RevokedAt = nil
	for _, other := range f.grants {
		if other == nil || other.ID == g.ID {
			continue
		}
		if other.GrantorUID != g.GrantorUID {
			continue
		}
		if other.Active != 1 {
			continue
		}
		// YUJ-1744 / PR#131 R4 — siblings are PAUSED, not REVOKED.
		// Skip rows the DELETE path already tombstoned (defensive),
		// and never stamp revoked_at on rows we demote here.
		if other.RevokedAt != nil {
			continue
		}
		other.Active = 0
		other.GlobalEnabled = 0
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

// createOrReactivateGrantAtomic — YUJ-1471 / PR#109 review blocker #2 + #3
// (restored after PR#121 R5 / B2 rebase regression). In-memory analogue
// of the prod transactional path. The fake's outer mu already
// serializes all writes, so the mutex semantics fall out naturally; we
// only need to express the (insert | reactivate) + demote sequence and
// the "reactivation always overwrites persona_prompt" invariant.
//
// Error injection: respects `failInsertGrant` so existing tests that
// model an insert failure continue to surface the same error class
// through the atomic API.
func (f *fakeOBOStore) createOrReactivateGrantAtomic(grantorUID, granteeBotUID, mode, personaPrompt string) (*oboGrantModel, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureInit()
	if grantorUID == "" || granteeBotUID == "" {
		return nil, false, errors.New("obo: grantor_uid and grantee_bot_uid are required")
	}
	if mode == "" {
		mode = "auto"
	}

	// Look for an existing (grantor, bot) row — same predicate the
	// prod UNIQUE KEY enforces.
	var existing *oboGrantModel
	for _, g := range f.grants {
		if g.GrantorUID == grantorUID && g.GranteeBotUID == granteeBotUID {
			existing = g
			break
		}
	}

	var (
		target      *oboGrantModel
		reactivated bool
	)

	if existing == nil {
		// Fresh insert path — honor the insert-failure injection hook so
		// tests that asserted insertGrant errors still see them here.
		if f.failInsertGrant != nil {
			return nil, false, f.failInsertGrant
		}
		f.nextID++
		id := f.nextID
		target = &oboGrantModel{
			ID:            id,
			GrantorUID:    grantorUID,
			GranteeBotUID: granteeBotUID,
			Mode:          mode,
			GlobalEnabled: 0,
			Active:        1,
			PersonaPrompt: personaPrompt,
		}
		f.grants[id] = target
	} else if existing.Active == 1 {
		// Live duplicate — surface the same 409 sentinel prod returns.
		return nil, false, errOBOGrantAlreadyActive
	} else {
		// Reactivation — always overwrite persona_prompt, including
		// when caller supplied "" (the "clear the prompt" signal).
		existing.Active = 1
		existing.GlobalEnabled = 0
		existing.RevokedAt = nil
		existing.PersonaPrompt = personaPrompt
		target = existing
		reactivated = true
	}

	// Demote every other active grant under the same grantor.
	// YUJ-1744 / PR#131 R4 — siblings demoted by create/reactivate are
	// PAUSED, not REVOKED. Leave revoked_at untouched (and skip rows a
	// prior DELETE already tombstoned, defensive).
	for _, g := range f.grants {
		if g.GrantorUID != grantorUID || g.ID == target.ID || g.Active != 1 {
			continue
		}
		if g.RevokedAt != nil {
			continue
		}
		g.Active = 0
		g.GlobalEnabled = 0
	}

	cp := *target
	return &cp, reactivated, nil
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
