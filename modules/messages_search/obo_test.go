package messages_search

import (
	"errors"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
)

// as-user(OBO) 鉴权接线单测（YUJ-53 / #F）。覆盖：
//   - 单频道门 = grantor 真人分支 ∩ OBO 已授 scope；
//   - 真人分支在先且短路（grantor 无权 → 不消费 OBO checker）；
//   - OBO scope 拒绝 → NOT_FOUND（存在性隐藏）；
//   - 双向 blacklist 对 grantor 生效；
//   - TOCTOU：grantor 失去实时成员资格 → 拒绝；
//   - checker 缺失 / infra 错 → fail-closed（INTERNAL），绝不降级为无 scope grantor 全量；
//   - global allowlist 与单频道门共用同一 OBO 谓词（决策九一致性）；
//   - App Bot 主体构造显式拒绝。

// fakeOBOChecker 是 oboChecker 的测试替身。allow[key] 命中 → 放行；err 非空 → infra 错。
type fakeOBOChecker struct {
	allow map[string]bool
	err   error
	calls int
	seen  []string // 记录被求值的 "channelID|type"，用于断言短路 / 频道维度
}

func oboKey(channelID string, channelType uint8) string {
	return channelID + "|" + string(rune('0'+channelType))
}

func (f *fakeOBOChecker) SearchOBOAllowed(botUID, grantorUID, channelID string, channelType uint8) (bool, error) {
	f.calls++
	f.seen = append(f.seen, oboKey(channelID, channelType))
	if f.err != nil {
		return false, f.err
	}
	return f.allow[oboKey(channelID, channelType)], nil
}

// newOBOHandler 组装带 authz 桩 + 注入 oboChecker 的 Handler。
func newOBOHandler(gSvc *stubAuthzGroupSvc, uSvc *stubAuthzUserSvc, chk oboChecker) *Handler {
	h := newAuthzHandlerFull(gSvc, uSvc, &stubAuthzThreadSvc{})
	h.oboCheck = chk
	return h
}

func oboGroupPrincipal() oboPrincipal {
	return oboPrincipal{botUID: "bot9", grantorUID: "grace", spaceID: ""}
}

// TestOBOCanReadChannel_GroupAllowed — grantor 是活跃成员且 OBO scope 已授 → 放行，
// 不写响应。经 canReadChannel dispatch 进入 obo 分支。
func TestOBOCanReadChannel_GroupAllowed(t *testing.T) {
	gSvc := &stubAuthzGroupSvc{
		groupModels:   map[string]*group.InfoResp{"G1": {GroupNo: "G1", Status: 1}, "G2": {GroupNo: "G2", Status: 1}},
		activeMembers: map[string]bool{"G1": true},
	}
	chk := &fakeOBOChecker{allow: map[string]bool{oboKey("G1", channelTypeGroup): true}}
	h := newOBOHandler(gSvc, &stubAuthzUserSvc{}, chk)
	c, rec := newAuthzCtx(t)
	p := oboGroupPrincipal()
	setPrincipal(c, p)

	if !h.canReadChannel(c, p, channelTypeGroup, "G1") {
		t.Fatalf("grantor member + obo scope granted must pass, body=%q", rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("allow must not write a response, got %q", rec.Body.String())
	}
	if chk.calls != 1 {
		t.Fatalf("obo checker must be consulted exactly once, got %d", chk.calls)
	}
}

// TestOBOCanReadChannel_ScopeDeniedHidesChannel — grantor 可达但 OBO 未授权该频道 →
// NOT_FOUND（存在性隐藏）。
func TestOBOCanReadChannel_ScopeDeniedHidesChannel(t *testing.T) {
	gSvc := &stubAuthzGroupSvc{
		groupModels:   map[string]*group.InfoResp{"G1": {GroupNo: "G1", Status: 1}, "G2": {GroupNo: "G2", Status: 1}},
		activeMembers: map[string]bool{"G1": true},
	}
	chk := &fakeOBOChecker{allow: map[string]bool{}} // scope 未授
	h := newOBOHandler(gSvc, &stubAuthzUserSvc{}, chk)
	c, rec := newAuthzCtx(t)
	p := oboGroupPrincipal()
	setPrincipal(c, p)

	if h.canReadChannel(c, p, channelTypeGroup, "G1") {
		t.Fatalf("obo scope not granted must be denied")
	}
	if !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("scope denial must render the not_found envelope, got %q", rec.Body.String())
	}
	if chk.calls != 1 {
		t.Fatalf("obo checker must be consulted once (real-person passed), got %d", chk.calls)
	}
}

// TestOBOCanReadChannel_RealPersonDeniedShortCircuits — grantor 非成员 → 真人分支先拒，
// OBO checker 根本不被消费（证明真人分支在先）。
func TestOBOCanReadChannel_RealPersonDeniedShortCircuits(t *testing.T) {
	gSvc := &stubAuthzGroupSvc{
		groupModels:   map[string]*group.InfoResp{"G1": {GroupNo: "G1", Status: 1}, "G2": {GroupNo: "G2", Status: 1}},
		activeMembers: map[string]bool{}, // 非成员
	}
	chk := &fakeOBOChecker{allow: map[string]bool{oboKey("G1", channelTypeGroup): true}}
	h := newOBOHandler(gSvc, &stubAuthzUserSvc{}, chk)
	c, rec := newAuthzCtx(t)
	p := oboGroupPrincipal()
	setPrincipal(c, p)

	if h.canReadChannel(c, p, channelTypeGroup, "G1") {
		t.Fatalf("non-member grantor must be denied")
	}
	if !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("real-person denial must render the not_found envelope, got %q", rec.Body.String())
	}
	if chk.calls != 0 {
		t.Fatalf("obo checker must NOT be consulted when real-person gate fails, got %d", chk.calls)
	}
}

// TestOBOCanReadChannel_TOCTOU_GrantorLostMembership — grantor 已被踢出群
// （ExistMemberActive=false）→ 立即拒绝，不放行陈旧 grant。TOCTOU 与发消息侧一致。
func TestOBOCanReadChannel_TOCTOU_GrantorLostMembership(t *testing.T) {
	gSvc := &stubAuthzGroupSvc{
		groupModels:   map[string]*group.InfoResp{"G1": {GroupNo: "G1", Status: 1}, "G2": {GroupNo: "G2", Status: 1}},
		activeMembers: map[string]bool{"G1": false}, // 已失去实时成员资格
	}
	chk := &fakeOBOChecker{allow: map[string]bool{oboKey("G1", channelTypeGroup): true}}
	h := newOBOHandler(gSvc, &stubAuthzUserSvc{}, chk)
	c, rec := newAuthzCtx(t)
	p := oboGroupPrincipal()
	setPrincipal(c, p)

	if h.canReadChannel(c, p, channelTypeGroup, "G1") {
		t.Fatalf("grantor who lost live membership must be denied (TOCTOU)")
	}
	if !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("TOCTOU denial must render the not_found envelope, got %q", rec.Body.String())
	}
}

// TestOBOCanReadChannel_DMBidirectionalBlacklist — grantor 与对端互为好友，但对端拉黑
// grantor → 双向 blacklist 生效，NOT_FOUND。obo 完全复用真人双向黑名单。
func TestOBOCanReadChannel_DMBidirectionalBlacklist(t *testing.T) {
	uSvc := &stubAuthzUserSvc{
		friends:    map[string]bool{friendKey("grace", "peer"): true},
		blacklists: map[string]bool{blacklistKey("peer", "grace"): true}, // peer→grantor 拉黑
		robots:     map[string]robotStub{"peer": {isRobot: false}},
	}
	chk := &fakeOBOChecker{allow: map[string]bool{oboKey("peer", channelTypePerson): true}}
	h := newOBOHandler(&stubAuthzGroupSvc{}, uSvc, chk)
	c, rec := newAuthzCtx(t)
	p := oboGroupPrincipal()
	setPrincipal(c, p)

	if h.canReadChannel(c, p, channelTypePerson, "peer") {
		t.Fatalf("bidirectional blacklist must hide the DM for obo grantor")
	}
	if !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("blacklist denial must render the not_found envelope, got %q", rec.Body.String())
	}
	if chk.calls != 0 {
		t.Fatalf("blacklist (real-person gate) must short-circuit before obo scope, got %d", chk.calls)
	}
}

// TestOBOCanReadChannel_NoCheckerFailsClosed — 路由未注入 oboChecker → INTERNAL，
// 绝不降级为无 scope 约束的 grantor 全量搜索。
func TestOBOCanReadChannel_NoCheckerFailsClosed(t *testing.T) {
	gSvc := &stubAuthzGroupSvc{
		groupModels:   map[string]*group.InfoResp{"G1": {GroupNo: "G1", Status: 1}, "G2": {GroupNo: "G2", Status: 1}},
		activeMembers: map[string]bool{"G1": true},
	}
	h := newOBOHandler(gSvc, &stubAuthzUserSvc{}, nil) // 无 checker
	c, rec := newAuthzCtx(t)
	p := oboGroupPrincipal()
	setPrincipal(c, p)

	if h.canReadChannel(c, p, channelTypeGroup, "G1") {
		t.Fatalf("missing obo checker must fail closed")
	}
	if !strings.Contains(rec.Body.String(), "Internal search error") {
		t.Fatalf("missing checker must render the internal-error envelope, got %q", rec.Body.String())
	}
}

// TestOBOCanReadChannel_CheckerErrorFailsClosed — OBO checker 返回 infra 错 → INTERNAL。
func TestOBOCanReadChannel_CheckerErrorFailsClosed(t *testing.T) {
	gSvc := &stubAuthzGroupSvc{
		groupModels:   map[string]*group.InfoResp{"G1": {GroupNo: "G1", Status: 1}, "G2": {GroupNo: "G2", Status: 1}},
		activeMembers: map[string]bool{"G1": true},
	}
	chk := &fakeOBOChecker{err: errors.New("obo store down")}
	h := newOBOHandler(gSvc, &stubAuthzUserSvc{}, chk)
	c, rec := newAuthzCtx(t)
	p := oboGroupPrincipal()
	setPrincipal(c, p)

	if h.canReadChannel(c, p, channelTypeGroup, "G1") {
		t.Fatalf("obo checker infra error must fail closed")
	}
	if !strings.Contains(rec.Body.String(), "Internal search error") {
		t.Fatalf("checker error must render the internal-error envelope, got %q", rec.Body.String())
	}
}

// TestFilterChannelsByOBO_NarrowsByScope — global allowlist 逐频道 ∩ OBO scope：仅保留
// 被授权的频道，且用每个频道自身 WireID + ChannelType 求值（子区用组合 id）。
func TestFilterChannelsByOBO_NarrowsByScope(t *testing.T) {
	threadID := thread.BuildChannelID("G2", "t123")
	refs := []channelRef{
		{OSChannelID: "G1", WireID: "G1", ChannelType: channelTypeGroup},
		{OSChannelID: "G2", WireID: "G2", ChannelType: channelTypeGroup},
		{OSChannelID: threadID, WireID: threadID, ChannelType: channelTypeThread},
	}
	chk := &fakeOBOChecker{allow: map[string]bool{
		oboKey("G1", channelTypeGroup):      true,
		oboKey(threadID, channelTypeThread): true,
		// G2 未授权 → 被剔除
	}}
	h := newOBOHandler(&stubAuthzGroupSvc{}, &stubAuthzUserSvc{}, chk)

	out, err := h.filterChannelsByOBO("bot9", "grace", refs)
	if err != nil {
		t.Fatalf("unexpected err %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 channels after scope narrowing, got %d (%+v)", len(out), out)
	}
	if out[0].WireID != "G1" || out[1].WireID != threadID {
		t.Fatalf("scope narrowing kept wrong channels: %+v", out)
	}
}

// TestFilterChannelsByOBO_FailClosedOnError — checker infra 错 → 整体返回 err（不部分放行）。
func TestFilterChannelsByOBO_FailClosedOnError(t *testing.T) {
	refs := []channelRef{{WireID: "G1", ChannelType: channelTypeGroup}}
	chk := &fakeOBOChecker{err: errors.New("boom")}
	h := newOBOHandler(&stubAuthzGroupSvc{}, &stubAuthzUserSvc{}, chk)

	if _, err := h.filterChannelsByOBO("bot9", "grace", refs); err == nil {
		t.Fatalf("infra error must fail closed (return err)")
	}
}

// TestOBOCrossPathConsistency — 决策九：任取频道，单频道门放行 ⇔ 该频道存活于 global
// allowlist 的 scope 收窄。两路共用同一 OBO 谓词（oboAllowed），故对同一 (wireID,type) 一致。
func TestOBOCrossPathConsistency(t *testing.T) {
	gSvc := &stubAuthzGroupSvc{
		groupModels:   map[string]*group.InfoResp{"G1": {GroupNo: "G1", Status: 1}, "G2": {GroupNo: "G2", Status: 1}},
		activeMembers: map[string]bool{"G1": true, "G2": true},
	}
	// G1 授权、G2 未授权。
	allow := map[string]bool{oboKey("G1", channelTypeGroup): true}
	refs := []channelRef{
		{WireID: "G1", ChannelType: channelTypeGroup},
		{WireID: "G2", ChannelType: channelTypeGroup},
	}
	p := oboGroupPrincipal()

	for _, ref := range refs {
		// 单频道门
		singleChk := &fakeOBOChecker{allow: allow}
		h1 := newOBOHandler(gSvc, &stubAuthzUserSvc{}, singleChk)
		c, _ := newAuthzCtx(t)
		setPrincipal(c, p)
		single := h1.canReadChannel(c, p, ref.ChannelType, ref.WireID)

		// allowlist 收窄
		listChk := &fakeOBOChecker{allow: allow}
		h2 := newOBOHandler(gSvc, &stubAuthzUserSvc{}, listChk)
		out, err := h2.filterChannelsByOBO(p.botUID, p.grantorUID, []channelRef{ref})
		if err != nil {
			t.Fatalf("filter err: %v", err)
		}
		inList := len(out) == 1

		if single != inList {
			t.Fatalf("cross-path drift for %s: single=%v inList=%v", ref.WireID, single, inList)
		}
	}
}

// TestAuthenticateOBOFromContext_AppBotDenied — App Bot 主体构造显式拒绝（不静默放行）。
func TestAuthenticateOBOFromContext_AppBotDenied(t *testing.T) {
	c, _ := newPrincipalCtx(t)
	c.Set(ctxKeyRobotID, "appbot1")
	c.Set(ctxKeyBotKind, botKindApp)

	_, err := authenticateOBOFromContext(c, "grace")
	if !errors.Is(err, errPrincipalAppBotDenied) {
		t.Fatalf("App Bot OBO must be explicitly denied, got %v", err)
	}
}

// TestAuthenticateOBOFromContext_UserBotAssembled — User Bot + grantor → oboPrincipal
// 载体（主体=grantor、审计记 botUID+grantor、限流按 botUID）。
func TestAuthenticateOBOFromContext_UserBotAssembled(t *testing.T) {
	c, _ := newPrincipalCtx(t)
	c.Set(ctxKeyRobotID, "bot9")
	c.Set(ctxKeyBotKind, botKindUser)

	p, err := authenticateOBOFromContext(c, "grace")
	if err != nil {
		t.Fatalf("unexpected err %v", err)
	}
	if p.Kind() != principalKindOBO {
		t.Fatalf("want obo kind, got %s", p.Kind())
	}
	if p.SubjectUID() != "grace" {
		t.Fatalf("subject must be grantor, got %q", p.SubjectUID())
	}
	if p.RateLimitKey() != "bot9" || p.AuditBotUID() != "bot9" || p.AuditGrantorUID() != "grace" {
		t.Fatalf("rate/audit mismatch: rl=%q auditBot=%q auditGrantor=%q",
			p.RateLimitKey(), p.AuditBotUID(), p.AuditGrantorUID())
	}
}

// TestAuthenticateOBOFromContext_MissingInputs — 缺 botUID / grantor → 未鉴权（fail-closed）。
func TestAuthenticateOBOFromContext_MissingInputs(t *testing.T) {
	// 缺 robot_id
	c1, _ := newPrincipalCtx(t)
	if _, err := authenticateOBOFromContext(c1, "grace"); !errors.Is(err, errPrincipalUnauthenticated) {
		t.Fatalf("missing bot uid must be unauthenticated, got %v", err)
	}
	// 缺 grantor
	c2, _ := newPrincipalCtx(t)
	c2.Set(ctxKeyRobotID, "bot9")
	if _, err := authenticateOBOFromContext(c2, ""); !errors.Is(err, errPrincipalUnauthenticated) {
		t.Fatalf("missing grantor must be unauthenticated, got %v", err)
	}
}
