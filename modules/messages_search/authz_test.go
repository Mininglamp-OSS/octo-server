package messages_search

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/gin-gonic/gin"
)

// stubAuthzGroupSvc fakes group.IService for the membership gate. The embedded
// interface is nil so any method other than ExistMemberActive /
// GetGroupWithGroupNo panics on call — proving checkChannelAccess only
// touches the methods the new contract requires.
type stubAuthzGroupSvc struct {
	group.IService
	activeMembers map[string]bool            // groupNo → caller is active member
	groupModels   map[string]*group.InfoResp // groupNo → snapshot (nil = not found)
	groupErr      error                      // GetGroupWithGroupNo error
	memberErr     error                      // ExistMemberActive error
	getGroupCalls int
	memberCalls   int
	gotGroupNo    string
	gotUID        string
}

func (s *stubAuthzGroupSvc) GetGroupWithGroupNo(groupNo string) (*group.InfoResp, error) {
	s.getGroupCalls++
	if s.groupErr != nil {
		return nil, s.groupErr
	}
	return s.groupModels[groupNo], nil
}

func (s *stubAuthzGroupSvc) ExistMemberActive(groupNo string, uid string) (bool, error) {
	s.memberCalls++
	s.gotGroupNo = groupNo
	s.gotUID = uid
	if s.memberErr != nil {
		return false, s.memberErr
	}
	return s.activeMembers[groupNo], nil
}

// stubAuthzUserSvc fakes user.IService for the p2p friend / blacklist gate,
// plus the bot-classification (QueryPeerRobotInfo) and same-Space
// (AreSpaceMembers) gates introduced for the Space-mode p2p access fix.
type stubAuthzUserSvc struct {
	user.IService
	friends      map[string]bool // "uid|peer" → friend?
	friendErr    error
	blacklists   map[string]bool // "from|to" → blocked?
	blacklistErr error
	robots       map[string]robotStub // peerUID → bot info
	robotErr     error
	spaceMembers map[string]bool // "spaceID|uid1|uid2" → both members?
	spaceErr     error
	friendCalls  int
	blCalls      int
	robotCalls   int
	spaceCalls   int
}

// robotStub mirrors the (isRobot, creatorUID) tuple returned by
// user.IService.QueryPeerRobotInfo, so a test can fake "peer is a bot
// created by X".
type robotStub struct {
	isRobot bool
	creator string
}

func friendKey(a, b string) string    { return a + "|" + b }
func blacklistKey(a, b string) string { return a + "|" + b }
func spaceKey(spaceID, a, b string) string {
	return spaceID + "|" + a + "|" + b
}

func (s *stubAuthzUserSvc) IsFriend(uid, toUID string) (bool, error) {
	s.friendCalls++
	if s.friendErr != nil {
		return false, s.friendErr
	}
	return s.friends[friendKey(uid, toUID)], nil
}

func (s *stubAuthzUserSvc) ExistBlacklist(uid, toUID string) (bool, error) {
	s.blCalls++
	if s.blacklistErr != nil {
		return false, s.blacklistErr
	}
	return s.blacklists[blacklistKey(uid, toUID)], nil
}

func (s *stubAuthzUserSvc) QueryPeerRobotInfo(peerUID string) (bool, string, error) {
	s.robotCalls++
	if s.robotErr != nil {
		return false, "", s.robotErr
	}
	r := s.robots[peerUID]
	return r.isRobot, r.creator, nil
}

func (s *stubAuthzUserSvc) AreSpaceMembers(spaceID, uid1, uid2 string) (bool, error) {
	s.spaceCalls++
	if s.spaceErr != nil {
		return false, s.spaceErr
	}
	return s.spaceMembers[spaceKey(spaceID, uid1, uid2)], nil
}

// stubAuthzThreadSvc fakes thread.IService for the thread gate. Only
// GetThread is implemented; everything else panics so the test asserts the
// gate doesn't drift onto adjacent thread methods.
type stubAuthzThreadSvc struct {
	thread.IService
	threadOK    map[string]bool // "groupNo|shortID" → exists?
	threadErr   error
	threadCalls int
}

func (s *stubAuthzThreadSvc) GetThread(groupNo, shortID, loginUID string) (*thread.ThreadResp, error) {
	s.threadCalls++
	if s.threadErr != nil {
		return nil, s.threadErr
	}
	if s.threadOK[groupNo+"|"+shortID] {
		return &thread.ThreadResp{}, nil
	}
	return nil, errors.New("thread not found")
}

func newAuthzCtx(t *testing.T) (*wkhttp.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest("POST", "/v1/messages/_search", nil)
	return &wkhttp.Context{Context: gc}, rec
}

// newAuthzCtxWithSpace builds an auth context with space_id pre-populated,
// matching what SpaceMiddleware would set in production. Used to exercise
// the same-Space branch of checkP2PAccess.
func newAuthzCtxWithSpace(t *testing.T, spaceID string) (*wkhttp.Context, *httptest.ResponseRecorder) {
	t.Helper()
	c, rec := newAuthzCtx(t)
	c.Set("space_id", spaceID)
	return c, rec
}

func newAuthzHandlerFull(gSvc group.IService, uSvc user.IService, tSvc thread.IService) *Handler {
	return &Handler{
		Log:           log.NewTLog("messages_search-authz-test"),
		cfg:           SearchConfig{},
		groupService:  gSvc,
		userService:   uSvc,
		threadService: tSvc,
	}
}

func newAuthzHandler(gSvc group.IService) *Handler {
	return newAuthzHandlerFull(gSvc, &stubAuthzUserSvc{}, &stubAuthzThreadSvc{})
}

// ---------- p2p ----------

// TestCheckChannelAccess_P2PFriendAllowed — friends with no blacklist either
// way pass the gate. Replaces the legacy "p2p always allowed" test (PR #361
// reviewer flag: search bypassed friend + blacklist).
func TestCheckChannelAccess_P2PFriendAllowed(t *testing.T) {
	uSvc := &stubAuthzUserSvc{
		friends:    map[string]bool{friendKey("me", "peer"): true},
		blacklists: map[string]bool{},
	}
	h := newAuthzHandlerFull(&stubAuthzGroupSvc{}, uSvc, &stubAuthzThreadSvc{})
	c, rec := newAuthzCtx(t)

	if !h.checkChannelAccess(c, channelTypePerson, "peer", "me") {
		t.Fatalf("friend with no blacklist must pass the p2p gate")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("no response should be written on allow, got %q", rec.Body.String())
	}
}

// TestCheckChannelAccess_P2PSelfAllowed — searching one's own DM (notes-to-self)
// must not consult friend/blacklist.
func TestCheckChannelAccess_P2PSelfAllowed(t *testing.T) {
	uSvc := &stubAuthzUserSvc{}
	h := newAuthzHandlerFull(&stubAuthzGroupSvc{}, uSvc, &stubAuthzThreadSvc{})
	c, _ := newAuthzCtx(t)

	if !h.checkChannelAccess(c, channelTypePerson, "me", "me") {
		t.Fatalf("self DM must always pass the p2p gate")
	}
	if uSvc.friendCalls != 0 || uSvc.blCalls != 0 {
		t.Fatalf("self DM must not call IsFriend/ExistBlacklist; got friend=%d bl=%d", uSvc.friendCalls, uSvc.blCalls)
	}
}

// TestCheckChannelAccess_P2PNotFriendDenied — non-friends are denied as
// NOT_FOUND (anti-enumeration).
func TestCheckChannelAccess_P2PNotFriendDenied(t *testing.T) {
	uSvc := &stubAuthzUserSvc{friends: map[string]bool{}}
	h := newAuthzHandlerFull(&stubAuthzGroupSvc{}, uSvc, &stubAuthzThreadSvc{})
	c, rec := newAuthzCtx(t)

	if h.checkChannelAccess(c, channelTypePerson, "peer", "me") {
		t.Fatalf("non-friend must be denied")
	}
	if uSvc.blCalls != 0 {
		t.Fatalf("blacklist must not be queried after non-friend rejection; got %d", uSvc.blCalls)
	}
	if !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("denial should render the not_found envelope, got %q", rec.Body.String())
	}
}

// TestCheckChannelAccess_P2PBlockedByMeDenied — caller blocking peer must
// hide history.
func TestCheckChannelAccess_P2PBlockedByMeDenied(t *testing.T) {
	uSvc := &stubAuthzUserSvc{
		friends:    map[string]bool{friendKey("me", "peer"): true},
		blacklists: map[string]bool{blacklistKey("me", "peer"): true},
	}
	h := newAuthzHandlerFull(&stubAuthzGroupSvc{}, uSvc, &stubAuthzThreadSvc{})
	c, rec := newAuthzCtx(t)

	if h.checkChannelAccess(c, channelTypePerson, "peer", "me") {
		t.Fatalf("caller-side blacklist must deny")
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("denial must write a response")
	}
}

// TestCheckChannelAccess_P2PBlockedByPeerDenied — peer blocking caller must
// hide history (anti-harassment).
func TestCheckChannelAccess_P2PBlockedByPeerDenied(t *testing.T) {
	uSvc := &stubAuthzUserSvc{
		friends:    map[string]bool{friendKey("me", "peer"): true},
		blacklists: map[string]bool{blacklistKey("peer", "me"): true},
	}
	h := newAuthzHandlerFull(&stubAuthzGroupSvc{}, uSvc, &stubAuthzThreadSvc{})
	c, rec := newAuthzCtx(t)

	if h.checkChannelAccess(c, channelTypePerson, "peer", "me") {
		t.Fatalf("peer-side blacklist must deny")
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("denial must write a response")
	}
}

// TestCheckChannelAccess_P2PIsFriendErrorFailsClosed — DB lookup errors must
// fail closed with INTERNAL_ERROR (not silently allow).
func TestCheckChannelAccess_P2PIsFriendErrorFailsClosed(t *testing.T) {
	uSvc := &stubAuthzUserSvc{friendErr: errors.New("db down")}
	h := newAuthzHandlerFull(&stubAuthzGroupSvc{}, uSvc, &stubAuthzThreadSvc{})
	c, rec := newAuthzCtx(t)

	if h.checkChannelAccess(c, channelTypePerson, "peer", "me") {
		t.Fatalf("IsFriend error must fail closed")
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("fail-closed denial must write a response")
	}
}

// TestCheckChannelAccess_P2PBlacklistErrorFailsClosed — blacklist lookup
// error must fail closed.
func TestCheckChannelAccess_P2PBlacklistErrorFailsClosed(t *testing.T) {
	uSvc := &stubAuthzUserSvc{
		friends:      map[string]bool{friendKey("me", "peer"): true},
		blacklistErr: errors.New("db down"),
	}
	h := newAuthzHandlerFull(&stubAuthzGroupSvc{}, uSvc, &stubAuthzThreadSvc{})
	c, rec := newAuthzCtx(t)

	if h.checkChannelAccess(c, channelTypePerson, "peer", "me") {
		t.Fatalf("ExistBlacklist error must fail closed")
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("fail-closed denial must write a response")
	}
}

// TestCheckChannelAccess_P2PSameSpaceNonFriendAllowed — Space-mode regression:
// in enterprise contact-book deployments the friend table is mostly empty,
// so a caller searching a coworker's DM is denied solely by friend → 404.
// Same-Space membership must allow the gate without requiring a friend row.
func TestCheckChannelAccess_P2PSameSpaceNonFriendAllowed(t *testing.T) {
	uSvc := &stubAuthzUserSvc{
		friends:      map[string]bool{}, // explicitly NOT friends
		spaceMembers: map[string]bool{spaceKey("S1", "me", "peer"): true},
	}
	h := newAuthzHandlerFull(&stubAuthzGroupSvc{}, uSvc, &stubAuthzThreadSvc{})
	c, rec := newAuthzCtxWithSpace(t, "S1")

	if !h.checkChannelAccess(c, channelTypePerson, "peer", "me") {
		t.Fatalf("same-Space coworkers must pass the gate even without a friend row")
	}
	if uSvc.spaceCalls != 1 {
		t.Fatalf("AreSpaceMembers must be consulted exactly once on the real-user path, got %d", uSvc.spaceCalls)
	}
	if uSvc.friendCalls != 0 {
		t.Fatalf("friend fallback must be skipped after Space allow, got friendCalls=%d", uSvc.friendCalls)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("no response should be written on allow, got %q", rec.Body.String())
	}
}

// TestCheckChannelAccess_P2PNoSpaceNonFriendDenied — without a Space and
// without a friend row, the real-user path must still 404 (legacy
// non-Space deployments rely on friend-only gating).
func TestCheckChannelAccess_P2PNoSpaceNonFriendDenied(t *testing.T) {
	uSvc := &stubAuthzUserSvc{friends: map[string]bool{}}
	h := newAuthzHandlerFull(&stubAuthzGroupSvc{}, uSvc, &stubAuthzThreadSvc{})
	c, rec := newAuthzCtx(t) // no space_id set

	if h.checkChannelAccess(c, channelTypePerson, "peer", "me") {
		t.Fatalf("no Space and not friends → must deny")
	}
	if uSvc.spaceCalls != 0 {
		t.Fatalf("AreSpaceMembers must be skipped when space_id is empty, got %d", uSvc.spaceCalls)
	}
	if uSvc.friendCalls != 1 {
		t.Fatalf("friend fallback must run exactly once, got %d", uSvc.friendCalls)
	}
	if !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("denial should render the not_found envelope, got %q", rec.Body.String())
	}
}

// TestCheckChannelAccess_P2POwnBotAllowed — caller searching their own bot's
// DM must short-circuit past Space, friend, and blacklist (a user can't
// meaningfully blacklist their own bot, and bots have no space_member row).
func TestCheckChannelAccess_P2POwnBotAllowed(t *testing.T) {
	uSvc := &stubAuthzUserSvc{
		robots: map[string]robotStub{"botX": {isRobot: true, creator: "me"}},
	}
	h := newAuthzHandlerFull(&stubAuthzGroupSvc{}, uSvc, &stubAuthzThreadSvc{})
	c, _ := newAuthzCtxWithSpace(t, "S1")

	if !h.checkChannelAccess(c, channelTypePerson, "botX", "me") {
		t.Fatalf("own bot must pass the gate")
	}
	if uSvc.robotCalls != 1 {
		t.Fatalf("QueryPeerRobotInfo must be consulted exactly once, got %d", uSvc.robotCalls)
	}
	if uSvc.spaceCalls != 0 || uSvc.friendCalls != 0 || uSvc.blCalls != 0 {
		t.Fatalf("own-bot path must skip Space/friend/blacklist; got space=%d friend=%d bl=%d",
			uSvc.spaceCalls, uSvc.friendCalls, uSvc.blCalls)
	}
}

// TestCheckChannelAccess_P2POtherBotNonFriendDenied — other user's bot is
// treated like a stranger: caller must be friends to search, matching
// robot module's "用户与Bot非好友关系，拒绝转发消息".
func TestCheckChannelAccess_P2POtherBotNonFriendDenied(t *testing.T) {
	uSvc := &stubAuthzUserSvc{
		robots:  map[string]robotStub{"botX": {isRobot: true, creator: "alice"}},
		friends: map[string]bool{}, // not friends
	}
	h := newAuthzHandlerFull(&stubAuthzGroupSvc{}, uSvc, &stubAuthzThreadSvc{})
	c, rec := newAuthzCtxWithSpace(t, "S1")

	if h.checkChannelAccess(c, channelTypePerson, "botX", "me") {
		t.Fatalf("other-user bot without friend relation must be denied")
	}
	if uSvc.spaceCalls != 0 {
		t.Fatalf("other-bot path must not consult AreSpaceMembers, got %d", uSvc.spaceCalls)
	}
	if uSvc.blCalls != 0 {
		t.Fatalf("denied other-bot path must not consult blacklist, got %d", uSvc.blCalls)
	}
	if !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("denial should render the not_found envelope, got %q", rec.Body.String())
	}
}

// TestCheckChannelAccess_P2POtherBotFriendAllowed — other user's bot WITH a
// friend row AND no blacklist passes (mirrors the read path: once added,
// a stranger's bot is conversational like any other peer, so it goes
// through the bidirectional blacklist gate the same way as a real user).
func TestCheckChannelAccess_P2POtherBotFriendAllowed(t *testing.T) {
	uSvc := &stubAuthzUserSvc{
		robots:     map[string]robotStub{"botX": {isRobot: true, creator: "alice"}},
		friends:    map[string]bool{friendKey("me", "botX"): true},
		blacklists: map[string]bool{},
	}
	h := newAuthzHandlerFull(&stubAuthzGroupSvc{}, uSvc, &stubAuthzThreadSvc{})
	c, rec := newAuthzCtxWithSpace(t, "S1")

	if !h.checkChannelAccess(c, channelTypePerson, "botX", "me") {
		t.Fatalf("other-user bot with friend relation and clean blacklist must pass")
	}
	if uSvc.blCalls != 2 {
		t.Fatalf("other-bot/friend path must consult blacklist bidirectionally, got %d", uSvc.blCalls)
	}
	if uSvc.spaceCalls != 0 {
		t.Fatalf("other-bot path must not consult AreSpaceMembers, got %d", uSvc.spaceCalls)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("no response should be written on allow, got %q", rec.Body.String())
	}
}

// TestCheckChannelAccess_P2POtherBotFriendBlacklisted — other user's bot
// that has been blacklisted (either direction) must be hidden from
// search, matching the legacy pre-Space behavior where a friend bot
// flowed through bidirectional blacklist just like a real-user peer.
// Regression guard for PR #469 review (option (a)).
func TestCheckChannelAccess_P2POtherBotFriendBlacklisted(t *testing.T) {
	t.Run("blocked_by_me", func(t *testing.T) {
		uSvc := &stubAuthzUserSvc{
			robots:     map[string]robotStub{"botX": {isRobot: true, creator: "alice"}},
			friends:    map[string]bool{friendKey("me", "botX"): true},
			blacklists: map[string]bool{blacklistKey("me", "botX"): true},
		}
		h := newAuthzHandlerFull(&stubAuthzGroupSvc{}, uSvc, &stubAuthzThreadSvc{})
		c, rec := newAuthzCtxWithSpace(t, "S1")

		if h.checkChannelAccess(c, channelTypePerson, "botX", "me") {
			t.Fatalf("other-user bot blocked by caller must be denied")
		}
		if !strings.Contains(rec.Body.String(), "not found") {
			t.Fatalf("denial should render the not_found envelope, got %q", rec.Body.String())
		}
	})
	t.Run("blocked_by_peer", func(t *testing.T) {
		uSvc := &stubAuthzUserSvc{
			robots:     map[string]robotStub{"botX": {isRobot: true, creator: "alice"}},
			friends:    map[string]bool{friendKey("me", "botX"): true},
			blacklists: map[string]bool{blacklistKey("botX", "me"): true},
		}
		h := newAuthzHandlerFull(&stubAuthzGroupSvc{}, uSvc, &stubAuthzThreadSvc{})
		c, rec := newAuthzCtxWithSpace(t, "S1")

		if h.checkChannelAccess(c, channelTypePerson, "botX", "me") {
			t.Fatalf("other-user bot that has blocked caller must be denied")
		}
		if !strings.Contains(rec.Body.String(), "not found") {
			t.Fatalf("denial should render the not_found envelope, got %q", rec.Body.String())
		}
	})
}

// TestCheckChannelAccess_P2PRobotInfoErrorFailsClosed — DB error on the
// bot-classification query must fail closed with INTERNAL_ERROR (not
// silently fall through to the real-user path: a bot misclassified as a
// real user could leak Space-mate access to someone else's bot).
func TestCheckChannelAccess_P2PRobotInfoErrorFailsClosed(t *testing.T) {
	uSvc := &stubAuthzUserSvc{robotErr: errors.New("db down")}
	h := newAuthzHandlerFull(&stubAuthzGroupSvc{}, uSvc, &stubAuthzThreadSvc{})
	c, rec := newAuthzCtxWithSpace(t, "S1")

	if h.checkChannelAccess(c, channelTypePerson, "peer", "me") {
		t.Fatalf("QueryPeerRobotInfo error must fail closed")
	}
	if uSvc.spaceCalls != 0 || uSvc.friendCalls != 0 {
		t.Fatalf("fail-closed must short-circuit before Space/friend; space=%d friend=%d",
			uSvc.spaceCalls, uSvc.friendCalls)
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("fail-closed denial must write a response")
	}
}

// TestCheckChannelAccess_P2PAreSpaceMembersErrorFailsClosed — DB error on
// the same-Space lookup must fail closed; we must not fall through to
// friend (a transient Space-DB outage shouldn't downgrade authorization
// to the empty friend table).
func TestCheckChannelAccess_P2PAreSpaceMembersErrorFailsClosed(t *testing.T) {
	uSvc := &stubAuthzUserSvc{
		spaceErr: errors.New("db down"),
		friends:  map[string]bool{friendKey("me", "peer"): true}, // would pass friend, but must NOT be consulted
	}
	h := newAuthzHandlerFull(&stubAuthzGroupSvc{}, uSvc, &stubAuthzThreadSvc{})
	c, rec := newAuthzCtxWithSpace(t, "S1")

	if h.checkChannelAccess(c, channelTypePerson, "peer", "me") {
		t.Fatalf("AreSpaceMembers error must fail closed")
	}
	if uSvc.friendCalls != 0 {
		t.Fatalf("Space error must not fall through to friend fallback, got %d friend calls", uSvc.friendCalls)
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("fail-closed denial must write a response")
	}
}

// TestCheckChannelAccess_P2PSameSpaceBlockedByMeDenied — blacklist overrides
// Space membership on the real-user path. Same-Space coworkers who have
// been blacklisted must still be hidden from search.
func TestCheckChannelAccess_P2PSameSpaceBlockedByMeDenied(t *testing.T) {
	uSvc := &stubAuthzUserSvc{
		spaceMembers: map[string]bool{spaceKey("S1", "me", "peer"): true},
		blacklists:   map[string]bool{blacklistKey("me", "peer"): true},
	}
	h := newAuthzHandlerFull(&stubAuthzGroupSvc{}, uSvc, &stubAuthzThreadSvc{})
	c, rec := newAuthzCtxWithSpace(t, "S1")

	if h.checkChannelAccess(c, channelTypePerson, "peer", "me") {
		t.Fatalf("blacklist must override Space membership")
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("denial must write a response")
	}
}

// ---------- group ----------

// TestCheckChannelAccess_GroupMemberAllowed — active members of a normal-status
// group pass.
func TestCheckChannelAccess_GroupMemberAllowed(t *testing.T) {
	gSvc := &stubAuthzGroupSvc{
		activeMembers: map[string]bool{"G1": true},
		groupModels: map[string]*group.InfoResp{
			"G1": {GroupNo: "G1", Status: 1},
		},
	}
	h := newAuthzHandler(gSvc)
	c, rec := newAuthzCtx(t)

	if !h.checkChannelAccess(c, channelTypeGroup, "G1", "me") {
		t.Fatalf("active member must pass the gate")
	}
	if gSvc.gotGroupNo != "G1" || gSvc.gotUID != "me" {
		t.Fatalf("membership checked with wrong identity: group=%q uid=%q", gSvc.gotGroupNo, gSvc.gotUID)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("no response should be written on allow, got %q", rec.Body.String())
	}
}

// TestCheckChannelAccess_GroupNonMemberDenied is the regression guard for the
// PR #361 blocking finding: any logged-in user could search ANY group's full
// message history by sending an arbitrary group_no.
func TestCheckChannelAccess_GroupNonMemberDenied(t *testing.T) {
	gSvc := &stubAuthzGroupSvc{
		activeMembers: map[string]bool{},
		groupModels: map[string]*group.InfoResp{
			"victim-group": {GroupNo: "victim-group", Status: 1},
		},
	}
	h := newAuthzHandler(gSvc)
	c, rec := newAuthzCtx(t)

	if h.checkChannelAccess(c, channelTypeGroup, "victim-group", "attacker") {
		t.Fatalf("non-member must be denied")
	}
	if gSvc.memberCalls != 1 {
		t.Fatalf("ExistMemberActive should be consulted exactly once, got %d", gSvc.memberCalls)
	}
	if !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("denial should render the not_found envelope, got %q", rec.Body.String())
	}
}

// TestCheckChannelAccess_GroupDisbandedAllowedForActiveMember — 企业微信式
// disband 语义（产品决策 2026-06）：解散群历史与搜索保持可读，活跃成员仍可访问。
// disband 不再短路成员校验——访问权交给 ExistMemberActive（移出/拉黑成员 is_deleted=1
// 或 status!=Normal，仍会被拒）。
func TestCheckChannelAccess_GroupDisbandedAllowedForActiveMember(t *testing.T) {
	gSvc := &stubAuthzGroupSvc{
		activeMembers: map[string]bool{"G1": true}, // 仍是活跃成员
		groupModels: map[string]*group.InfoResp{
			"G1": {GroupNo: "G1", Status: group.GroupStatusDisband},
		},
	}
	h := newAuthzHandler(gSvc)
	c, _ := newAuthzCtx(t)

	if !h.checkChannelAccess(c, channelTypeGroup, "G1", "me") {
		t.Fatalf("解散群的活跃成员必须被放行（历史/搜索可读）")
	}
	if gSvc.memberCalls == 0 {
		t.Fatalf("disband 不再短路：必须仍调用 ExistMemberActive 把关成员资格")
	}
}

// TestCheckChannelAccess_GroupDisbandedDeniedForNonMember — 解散后被移出/非成员
// 仍应被拒（ExistMemberActive 返回 false），不能借残留 group_member 行越权读。
func TestCheckChannelAccess_GroupDisbandedDeniedForNonMember(t *testing.T) {
	gSvc := &stubAuthzGroupSvc{
		activeMembers: map[string]bool{}, // 非活跃成员（已移出 / 被拉黑）
		groupModels: map[string]*group.InfoResp{
			"G1": {GroupNo: "G1", Status: group.GroupStatusDisband},
		},
	}
	h := newAuthzHandler(gSvc)
	c, rec := newAuthzCtx(t)

	if h.checkChannelAccess(c, channelTypeGroup, "G1", "me") {
		t.Fatalf("解散群的非成员必须被拒")
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("denial must write a response")
	}
}

// TestCheckChannelAccess_GroupNilModelDenied — a nil group model is treated
// as "does not exist" (not an internal error), to match anti-enumeration.
func TestCheckChannelAccess_GroupNilModelDenied(t *testing.T) {
	gSvc := &stubAuthzGroupSvc{
		activeMembers: map[string]bool{},
		groupModels:   map[string]*group.InfoResp{}, // nil for any groupNo
	}
	h := newAuthzHandler(gSvc)
	c, rec := newAuthzCtx(t)

	if h.checkChannelAccess(c, channelTypeGroup, "G1", "me") {
		t.Fatalf("missing group model must be denied")
	}
	if !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("denial should render the not_found envelope, got %q", rec.Body.String())
	}
}

// TestCheckChannelAccess_GroupGetGroupErrorFailsClosed — GetGroupWithGroupNo
// error must fail closed.
func TestCheckChannelAccess_GroupGetGroupErrorFailsClosed(t *testing.T) {
	gSvc := &stubAuthzGroupSvc{groupErr: errors.New("db down")}
	h := newAuthzHandler(gSvc)
	c, rec := newAuthzCtx(t)

	if h.checkChannelAccess(c, channelTypeGroup, "G1", "me") {
		t.Fatalf("GetGroupWithGroupNo error must fail closed")
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("fail-closed denial must write a response")
	}
}

// TestCheckChannelAccess_GroupExistMemberErrorFailsClosed — membership lookup
// error must deny (legacy regression: covers the original
// LookupErrorFailsClosed case from the pre-disband code path).
func TestCheckChannelAccess_GroupExistMemberErrorFailsClosed(t *testing.T) {
	gSvc := &stubAuthzGroupSvc{
		groupModels: map[string]*group.InfoResp{
			"G1": {GroupNo: "G1", Status: 1},
		},
		memberErr: errors.New("db down"),
	}
	h := newAuthzHandler(gSvc)
	c, rec := newAuthzCtx(t)

	if h.checkChannelAccess(c, channelTypeGroup, "G1", "me") {
		t.Fatalf("ExistMemberActive error must fail closed")
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("fail-closed denial must write a response")
	}
}

// ---------- thread ----------

// TestCheckChannelAccess_ThreadMemberAllowed — parent-group active member
// with an existing (non-deleted) thread passes.
func TestCheckChannelAccess_ThreadMemberAllowed(t *testing.T) {
	gSvc := &stubAuthzGroupSvc{activeMembers: map[string]bool{"G9": true}}
	tSvc := &stubAuthzThreadSvc{threadOK: map[string]bool{"G9|123456789012345": true}}
	h := newAuthzHandlerFull(gSvc, &stubAuthzUserSvc{}, tSvc)
	c, _ := newAuthzCtx(t)

	if !h.checkChannelAccess(c, channelTypeThread, "G9____123456789012345", "me") {
		t.Fatalf("parent-group member of an existing thread must pass the gate")
	}
	if gSvc.gotGroupNo != "G9" {
		t.Fatalf("thread gate must check the parent group, got %q", gSvc.gotGroupNo)
	}
	if tSvc.threadCalls != 1 {
		t.Fatalf("thread gate must consult GetThread exactly once, got %d", tSvc.threadCalls)
	}
}

// TestCheckChannelAccess_ThreadNonMemberDenied — non-members of the parent
// group are denied.
func TestCheckChannelAccess_ThreadNonMemberDenied(t *testing.T) {
	gSvc := &stubAuthzGroupSvc{activeMembers: map[string]bool{}}
	tSvc := &stubAuthzThreadSvc{threadOK: map[string]bool{"G9|123456789012345": true}}
	h := newAuthzHandlerFull(gSvc, &stubAuthzUserSvc{}, tSvc)
	c, rec := newAuthzCtx(t)

	if h.checkChannelAccess(c, channelTypeThread, "G9____123456789012345", "attacker") {
		t.Fatalf("non-member of parent group must be denied thread search")
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("denial must write a response")
	}
}

// TestCheckChannelAccess_ThreadMalformedIDDenied — malformed channel_id is
// rejected before any service lookup.
func TestCheckChannelAccess_ThreadMalformedIDDenied(t *testing.T) {
	gSvc := &stubAuthzGroupSvc{activeMembers: map[string]bool{"": true}}
	tSvc := &stubAuthzThreadSvc{}
	h := newAuthzHandlerFull(gSvc, &stubAuthzUserSvc{}, tSvc)
	c, rec := newAuthzCtx(t)

	if h.checkChannelAccess(c, channelTypeThread, "____orphan", "me") {
		t.Fatalf("malformed thread channel_id must be denied")
	}
	if gSvc.memberCalls != 0 || tSvc.threadCalls != 0 {
		t.Fatalf("malformed id must short-circuit; group=%d thread=%d", gSvc.memberCalls, tSvc.threadCalls)
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("denial must write a response")
	}
}

// TestCheckChannelAccess_ThreadDeletedOrMissingDenied — when the thread is
// missing or deleted, GetThread returns err and the gate denies before
// consulting the group membership (anti-enumeration).
func TestCheckChannelAccess_ThreadDeletedOrMissingDenied(t *testing.T) {
	gSvc := &stubAuthzGroupSvc{activeMembers: map[string]bool{"G9": true}}
	tSvc := &stubAuthzThreadSvc{threadOK: map[string]bool{}} // GetThread always errors → not found
	h := newAuthzHandlerFull(gSvc, &stubAuthzUserSvc{}, tSvc)
	c, rec := newAuthzCtx(t)

	if h.checkChannelAccess(c, channelTypeThread, "G9____123456789012345", "me") {
		t.Fatalf("missing/deleted thread must be denied")
	}
	if gSvc.memberCalls != 0 {
		t.Fatalf("ExistMemberActive must not be reached after GetThread err; got %d", gSvc.memberCalls)
	}
	if !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("denial should render the not_found envelope, got %q", rec.Body.String())
	}
}

// TestCheckChannelAccess_ThreadGetThreadDBErrorDenied — DB error inside
// GetThread collapses with not-found into NOT_FOUND (anti-enumeration over
// operational signal).
func TestCheckChannelAccess_ThreadGetThreadDBErrorDenied(t *testing.T) {
	gSvc := &stubAuthzGroupSvc{activeMembers: map[string]bool{"G9": true}}
	tSvc := &stubAuthzThreadSvc{threadErr: errors.New("db down")}
	h := newAuthzHandlerFull(gSvc, &stubAuthzUserSvc{}, tSvc)
	c, rec := newAuthzCtx(t)

	if h.checkChannelAccess(c, channelTypeThread, "G9____123456789012345", "me") {
		t.Fatalf("GetThread DB error must deny")
	}
	if !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("denial should render the not_found envelope, got %q", rec.Body.String())
	}
}

// TestCheckChannelAccess_ThreadParentMemberErrorFailsClosed — a DB error on
// ExistMemberActive (after GetThread succeeded) maps to INTERNAL_ERROR,
// not NOT_FOUND, because the existence question has already been answered.
func TestCheckChannelAccess_ThreadParentMemberErrorFailsClosed(t *testing.T) {
	gSvc := &stubAuthzGroupSvc{
		memberErr: errors.New("db down"),
	}
	tSvc := &stubAuthzThreadSvc{threadOK: map[string]bool{"G9|123456789012345": true}}
	h := newAuthzHandlerFull(gSvc, &stubAuthzUserSvc{}, tSvc)
	c, rec := newAuthzCtx(t)

	if h.checkChannelAccess(c, channelTypeThread, "G9____123456789012345", "me") {
		t.Fatalf("parent-group ExistMemberActive error must fail closed")
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("fail-closed denial must write a response")
	}
}
