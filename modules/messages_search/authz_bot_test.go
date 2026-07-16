package messages_search

import (
	"errors"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/modules/group"
)

// as-bot 群/子区单频道门（#D / YUJ-51）。验收口径：
//   - as-bot：所在群 / 子区历史可搜；非成员 / 被移出 / 被拉黑 → 搜不到（NOT_FOUND，反枚举）。
//   - 主体是 botUID：ExistMemberActive 必须以 botUID 求值（bot 有自己的 group_member 行）。
//   - 归一化（决策九）：群/子区门复用真人 checkGroupAccess / checkThreadAccess，仅主体 uid
//     换成 botUID——与 #E buildBotAllowlist 的 ExistMembersActive 枚举同源，不另写一套规则。
//   - as-bot 群/子区门不触碰 P2P 机器（friend / blacklist / space），那是 DM 门（#C）的事。

// TestBotCanReadChannel_GroupMemberAllowed — a bot that is an active member of
// a normal-status group passes, and membership is checked against the botUID
// (bot has its own group_member row), not any human identity.
func TestBotCanReadChannel_GroupMemberAllowed(t *testing.T) {
	gSvc := &stubAuthzGroupSvc{
		activeMembers: map[string]bool{"G1": true},
		groupModels:   map[string]*group.InfoResp{"G1": {GroupNo: "G1", Status: 1}},
	}
	uSvc := &stubAuthzUserSvc{}
	h := newAuthzHandlerFull(gSvc, uSvc, &stubAuthzThreadSvc{})
	c, rec := newAuthzCtx(t)

	if !h.canReadChannel(c, userBotPrincipal{botUID: "bot9"}, channelTypeGroup, "G1") {
		t.Fatalf("as-bot active group member must pass the gate")
	}
	if gSvc.gotUID != "bot9" || gSvc.gotGroupNo != "G1" {
		t.Fatalf("group membership must be checked with the botUID; got group=%q uid=%q", gSvc.gotGroupNo, gSvc.gotUID)
	}
	// The group gate must not drift onto the P2P machinery (friend/blacklist/space).
	if uSvc.friendCalls != 0 || uSvc.blCalls != 0 || uSvc.spaceCalls != 0 || uSvc.robotCalls != 0 {
		t.Fatalf("as-bot group gate must not consult P2P services; friend=%d bl=%d space=%d robot=%d",
			uSvc.friendCalls, uSvc.blCalls, uSvc.spaceCalls, uSvc.robotCalls)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("no response should be written on allow, got %q", rec.Body.String())
	}
}

// TestBotCanReadChannel_GroupNonMemberDenied — a bot that is NOT an active
// member (kicked / group-blacklisted → ExistMemberActive false, including the
// #354 cascade that flips an owner-blacklisted user's in-group bot to
// status!=Normal) must be denied with NOT_FOUND (anti-enumeration).
func TestBotCanReadChannel_GroupNonMemberDenied(t *testing.T) {
	gSvc := &stubAuthzGroupSvc{
		activeMembers: map[string]bool{}, // bot not active (removed / group-blacklisted)
		groupModels:   map[string]*group.InfoResp{"G1": {GroupNo: "G1", Status: 1}},
	}
	h := newAuthzHandlerFull(gSvc, &stubAuthzUserSvc{}, &stubAuthzThreadSvc{})
	c, rec := newAuthzCtx(t)

	if h.canReadChannel(c, userBotPrincipal{botUID: "bot9"}, channelTypeGroup, "G1") {
		t.Fatalf("as-bot non-member (or group-blacklisted) must be denied")
	}
	if gSvc.memberCalls != 1 || gSvc.gotUID != "bot9" {
		t.Fatalf("ExistMemberActive must be consulted once with botUID; calls=%d uid=%q", gSvc.memberCalls, gSvc.gotUID)
	}
	if !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("denial should render the not_found envelope, got %q", rec.Body.String())
	}
}

// TestBotCanReadChannel_GroupMissingDenied — a missing group model is treated
// as "does not exist" → NOT_FOUND, before any membership lookup.
func TestBotCanReadChannel_GroupMissingDenied(t *testing.T) {
	gSvc := &stubAuthzGroupSvc{
		activeMembers: map[string]bool{"G1": true}, // would pass membership, but group is gone
		groupModels:   map[string]*group.InfoResp{},
	}
	h := newAuthzHandlerFull(gSvc, &stubAuthzUserSvc{}, &stubAuthzThreadSvc{})
	c, rec := newAuthzCtx(t)

	if h.canReadChannel(c, userBotPrincipal{botUID: "bot9"}, channelTypeGroup, "G1") {
		t.Fatalf("as-bot search of a missing group must be denied")
	}
	if gSvc.memberCalls != 0 {
		t.Fatalf("missing group must short-circuit before ExistMemberActive; got %d", gSvc.memberCalls)
	}
	if !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("denial should render the not_found envelope, got %q", rec.Body.String())
	}
}

// TestBotCanReadChannel_GroupMemberErrorFailsClosed — an ExistMemberActive DB
// error must fail closed (deny), never silently allow.
func TestBotCanReadChannel_GroupMemberErrorFailsClosed(t *testing.T) {
	gSvc := &stubAuthzGroupSvc{
		groupModels: map[string]*group.InfoResp{"G1": {GroupNo: "G1", Status: 1}},
		memberErr:   errors.New("db down"),
	}
	h := newAuthzHandlerFull(gSvc, &stubAuthzUserSvc{}, &stubAuthzThreadSvc{})
	c, rec := newAuthzCtx(t)

	if h.canReadChannel(c, userBotPrincipal{botUID: "bot9"}, channelTypeGroup, "G1") {
		t.Fatalf("as-bot ExistMemberActive error must fail closed")
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("fail-closed denial must write a response")
	}
}

// TestBotCanReadChannel_ThreadMemberAllowed — a bot that is an active member of
// the parent group can search an existing thread (子区继承父群成员身份). The
// parent-group membership is checked against the botUID.
func TestBotCanReadChannel_ThreadMemberAllowed(t *testing.T) {
	gSvc := &stubAuthzGroupSvc{activeMembers: map[string]bool{"G9": true}}
	tSvc := &stubAuthzThreadSvc{threadOK: map[string]bool{"G9|123456789012345": true}}
	h := newAuthzHandlerFull(gSvc, &stubAuthzUserSvc{}, tSvc)
	c, rec := newAuthzCtx(t)

	if !h.canReadChannel(c, userBotPrincipal{botUID: "bot9"}, channelTypeThread, "G9____123456789012345") {
		t.Fatalf("as-bot parent-group member must pass the thread gate")
	}
	if gSvc.gotGroupNo != "G9" || gSvc.gotUID != "bot9" {
		t.Fatalf("thread gate must check the parent group with botUID; got group=%q uid=%q", gSvc.gotGroupNo, gSvc.gotUID)
	}
	if tSvc.threadCalls != 1 {
		t.Fatalf("thread gate must consult GetThread exactly once, got %d", tSvc.threadCalls)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("no response should be written on allow, got %q", rec.Body.String())
	}
}

// TestBotCanReadChannel_ThreadNonMemberDenied — a bot that is not an active
// member of the parent group is denied (NOT_FOUND), same as a real non-member.
func TestBotCanReadChannel_ThreadNonMemberDenied(t *testing.T) {
	gSvc := &stubAuthzGroupSvc{activeMembers: map[string]bool{}}
	tSvc := &stubAuthzThreadSvc{threadOK: map[string]bool{"G9|123456789012345": true}}
	h := newAuthzHandlerFull(gSvc, &stubAuthzUserSvc{}, tSvc)
	c, rec := newAuthzCtx(t)

	if h.canReadChannel(c, userBotPrincipal{botUID: "bot9"}, channelTypeThread, "G9____123456789012345") {
		t.Fatalf("as-bot non-member of parent group must be denied thread search")
	}
	if !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("denial should render the not_found envelope, got %q", rec.Body.String())
	}
}

// TestBotCanReadChannel_ThreadMissingDenied — a missing/deleted thread is
// denied (NOT_FOUND) before the parent-group membership lookup runs.
func TestBotCanReadChannel_ThreadMissingDenied(t *testing.T) {
	gSvc := &stubAuthzGroupSvc{activeMembers: map[string]bool{"G9": true}}
	tSvc := &stubAuthzThreadSvc{threadOK: map[string]bool{}} // GetThread always errors → not found
	h := newAuthzHandlerFull(gSvc, &stubAuthzUserSvc{}, tSvc)
	c, rec := newAuthzCtx(t)

	if h.canReadChannel(c, userBotPrincipal{botUID: "bot9"}, channelTypeThread, "G9____123456789012345") {
		t.Fatalf("as-bot search of a missing/deleted thread must be denied")
	}
	if gSvc.memberCalls != 0 {
		t.Fatalf("ExistMemberActive must not be reached after GetThread err; got %d", gSvc.memberCalls)
	}
	if !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("denial should render the not_found envelope, got %q", rec.Body.String())
	}
}
