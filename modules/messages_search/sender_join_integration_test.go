package messages_search

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
)

// stubUserSvc implements just enough of user.IService for senderJoin tests.
// Embedded interface is nil so any method we don't override panics on call —
// senderJoin only ever needs GetUsers.
type stubUserSvc struct {
	user.IService
	users  []*user.Resp
	err    error
	calls  int
	gotIDs []string

	// GetFriends stub — only populated by the allowlist tests.
	friends    []*user.FriendResp
	friendsErr error

	// ExistBlacklist stub — only populated by the DM-allowlist tests.
	// key = fromUID + "->" + toUID; missing key defaults to false (not blocked).
	blacklist    map[string]bool
	blacklistErr error
}

func (s *stubUserSvc) GetUsers(uids []string) ([]*user.Resp, error) {
	s.calls++
	s.gotIDs = append(s.gotIDs, uids...)
	return s.users, s.err
}

func (s *stubUserSvc) GetFriends(uid string) ([]*user.FriendResp, error) {
	return s.friends, s.friendsErr
}

// ExistBlacklist returns whether `uid` has blacklisted `toUID`. Default (both
// zero-value map and uninitialised field) is false so tests that never touch
// the blacklist axis keep passing.
func (s *stubUserSvc) ExistBlacklist(uid, toUID string) (bool, error) {
	if s.blacklistErr != nil {
		return false, s.blacklistErr
	}
	if s.blacklist == nil {
		return false, nil
	}
	return s.blacklist[uid+"->"+toUID], nil
}

// stubGroupSvc same idea for group.IService.GetMembers.
type stubGroupSvc struct {
	group.IService
	members []*group.MemberResp
	err     error
	calls   int

	// Group allowlist / active-status stubs.
	groupsForMember    map[string][]*group.InfoResp // uid -> groups
	groupsForMemberErr error
	// activeGroupsForMember[uid] returns the subset of groupNos considered
	// active for uid; nil map means "all groupNos are active" (default).
	activeGroupsForMember    map[string]map[string]bool
	activeGroupsForMemberErr error
}

func (s *stubGroupSvc) GetMembers(groupNo string) ([]*group.MemberResp, error) {
	s.calls++
	return s.members, s.err
}

// GetGroupsWithMemberUID returns the caller's groups, mirroring the
// production interface but with a fixture-only map.
func (s *stubGroupSvc) GetGroupsWithMemberUID(uid string) ([]*group.InfoResp, error) {
	if s.groupsForMemberErr != nil {
		return nil, s.groupsForMemberErr
	}
	if s.groupsForMember == nil {
		return nil, nil
	}
	return s.groupsForMember[uid], nil
}

// ExistMembersActive returns the subset of groupNos where uid is an active
// member. Default (nil map) is "every requested groupNo is active" so tests
// that don't care about the blacklist axis keep passing unchanged.
func (s *stubGroupSvc) ExistMembersActive(groupNos []string, uid string) ([]string, error) {
	if s.activeGroupsForMemberErr != nil {
		return nil, s.activeGroupsForMemberErr
	}
	if s.activeGroupsForMember == nil {
		return append([]string(nil), groupNos...), nil
	}
	allowed := s.activeGroupsForMember[uid]
	if allowed == nil {
		return append([]string(nil), groupNos...), nil
	}
	out := make([]string, 0, len(groupNos))
	for _, no := range groupNos {
		if allowed[no] {
			out = append(out, no)
		}
	}
	return out, nil
}

// newTestHandler constructs a Handler suitable for senderJoin testing. Cache
// has a 5-minute TTL so the regression cases are stable; cfg is empty so
// buildUserAvatarURL returns the relative-path fallback.
func newTestHandler(uSvc user.IService, gSvc group.IService) *Handler {
	return &Handler{
		Log:          log.NewTLog("messages_search-test"),
		cfg:          SearchConfig{},
		userService:  uSvc,
		groupService: gSvc,
		cache:        newSenderCache(64, 5*time.Minute),
	}
}

func TestSenderJoin_DM_UsesUserName(t *testing.T) {
	uSvc := &stubUserSvc{
		users: []*user.Resp{{UID: "u1", Name: "Alice"}},
	}
	gSvc := &stubGroupSvc{}
	h := newTestHandler(uSvc, gSvc)

	got := h.senderJoin(context.Background(), []string{"u1"}, channelTypePerson, "peer")
	if got.Names["u1"] != "Alice" {
		t.Fatalf("DM should use user.Name, got %q", got.Names["u1"])
	}
	if got.Avatars["u1"] != "users/u1/avatar" {
		t.Fatalf("DM avatar: got %q", got.Avatars["u1"])
	}
	if gSvc.calls != 0 {
		t.Fatalf("DM path must not call GetMembers; got calls=%d", gSvc.calls)
	}
}

func TestSenderJoin_Group_PrefersRemark(t *testing.T) {
	uSvc := &stubUserSvc{
		users: []*user.Resp{{UID: "u1", Name: "Alice"}},
	}
	gSvc := &stubGroupSvc{
		members: []*group.MemberResp{{UID: "u1", Remark: "Boss"}},
	}
	h := newTestHandler(uSvc, gSvc)

	got := h.senderJoin(context.Background(), []string{"u1"}, channelTypeGroup, "G1")
	if got.Names["u1"] != "Boss" {
		t.Fatalf("group should prefer remark, got %q", got.Names["u1"])
	}
}

func TestSenderJoin_Group_FallsBackToUserNameWhenNoRemark(t *testing.T) {
	uSvc := &stubUserSvc{
		users: []*user.Resp{{UID: "u1", Name: "Alice"}},
	}
	gSvc := &stubGroupSvc{
		members: []*group.MemberResp{{UID: "u1", Remark: ""}}, // empty remark
	}
	h := newTestHandler(uSvc, gSvc)

	got := h.senderJoin(context.Background(), []string{"u1"}, channelTypeGroup, "G1")
	if got.Names["u1"] != "Alice" {
		t.Fatalf("empty remark should fall back to user.Name, got %q", got.Names["u1"])
	}
}

func TestSenderJoin_GetMembersFailFallsBackToUserName(t *testing.T) {
	uSvc := &stubUserSvc{
		users: []*user.Resp{{UID: "u1", Name: "Alice"}},
	}
	gSvc := &stubGroupSvc{err: errors.New("db down")}
	h := newTestHandler(uSvc, gSvc)

	got := h.senderJoin(context.Background(), []string{"u1"}, channelTypeGroup, "G1")
	if got.Names["u1"] != "Alice" {
		t.Fatalf("GetMembers error should fall back to user.Name, got %q", got.Names["u1"])
	}
}

func TestSenderJoin_GetUsersFailReturnsEmpty(t *testing.T) {
	uSvc := &stubUserSvc{err: errors.New("user db down")}
	gSvc := &stubGroupSvc{}
	h := newTestHandler(uSvc, gSvc)

	got := h.senderJoin(context.Background(), []string{"u1"}, channelTypePerson, "peer")
	if name, ok := got.Names["u1"]; ok {
		t.Fatalf("GetUsers error must not surface a name, got %q", name)
	}
	if avatar, ok := got.Avatars["u1"]; ok {
		t.Fatalf("GetUsers error must not surface an avatar, got %q", avatar)
	}
}

// TestSenderJoin_CacheHitsSkipDB asserts the LRU short-circuits the DB path
// when the same uid is queried twice within TTL.
func TestSenderJoin_CacheHitsSkipDB(t *testing.T) {
	uSvc := &stubUserSvc{
		users: []*user.Resp{{UID: "u1", Name: "Alice"}},
	}
	gSvc := &stubGroupSvc{}
	h := newTestHandler(uSvc, gSvc)

	_ = h.senderJoin(context.Background(), []string{"u1"}, channelTypePerson, "peer")
	if uSvc.calls != 1 {
		t.Fatalf("first call should hit DB once, got calls=%d", uSvc.calls)
	}
	_ = h.senderJoin(context.Background(), []string{"u1"}, channelTypePerson, "peer")
	if uSvc.calls != 1 {
		t.Fatalf("second call should hit cache, got calls=%d", uSvc.calls)
	}
}

// TestSenderJoin_ScopeIsolation_GroupVsDM is the integration variant of
// TestSenderCache_ScopedKeys — exercises the DB → cache → wire path end to
// end and proves G1's "Boss" remark cannot leak into a DM render.
func TestSenderJoin_ScopeIsolation_GroupVsDM(t *testing.T) {
	uSvc := &stubUserSvc{
		users: []*user.Resp{{UID: "u1", Name: "Alice"}},
	}
	gSvc := &stubGroupSvc{
		members: []*group.MemberResp{{UID: "u1", Remark: "Boss"}},
	}
	h := newTestHandler(uSvc, gSvc)

	got := h.senderJoin(context.Background(), []string{"u1"}, channelTypeGroup, "G1")
	if got.Names["u1"] != "Boss" {
		t.Fatalf("G1 should resolve to Boss, got %q", got.Names["u1"])
	}

	// Same handler, same uid, but DM scope — the cache key is "u:u1" which
	// is different from "g:G1:u1", so we should NOT pick up "Boss". Reset
	// the group stub so a leak would pass through user.Name as "Alice"; the
	// only way to see "Boss" here is via the cache.
	gSvc.members = nil
	got = h.senderJoin(context.Background(), []string{"u1"}, channelTypePerson, "peer")
	if got.Names["u1"] != "Alice" {
		t.Fatalf("DM must not inherit group remark; got %q", got.Names["u1"])
	}
}
