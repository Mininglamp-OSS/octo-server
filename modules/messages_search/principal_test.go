package messages_search

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/gin-gonic/gin"
)

// newPrincipalCtx builds a bare wkhttp.Context whose gin context keys can be
// set to mimic what each auth middleware would populate (real-user
// AuthMiddleware → "uid"/"space_id"; authBot → "robot_id"/"bot_kind";
// authUserAPIKey → "api_key_uid"/"api_key_space_id").
func newPrincipalCtx(t *testing.T) (*wkhttp.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest("POST", "/v1/messages/_search", nil)
	return &wkhttp.Context{Context: gc}, rec
}

// ---------- carrier: three-state (+user) value coverage ----------

// TestPrincipalCarrier_SubjectUID covers 决策十 acceptance "principal 载体单测覆盖
// 三态取值": SubjectUID resolves to loginUID / botUID / grantorUID / keyUID per kind.
func TestPrincipalCarrier_SubjectUID(t *testing.T) {
	cases := []struct {
		name string
		p    Principal
		want string
	}{
		{"user", userPrincipal{uid: "alice", spaceID: "S1"}, "alice"},
		{"user_bot", userBotPrincipal{botUID: "bot9"}, "bot9"},
		{"obo", oboPrincipal{botUID: "bot9", grantorUID: "grace", spaceID: "S1"}, "grace"},
		{"uk", ukPrincipal{keyUID: "kate", spaceID: "S2"}, "kate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.SubjectUID(); got != tc.want {
				t.Fatalf("SubjectUID = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPrincipalCarrier_SpaceID — user/obo/uk carry a Space; user_bot never does
// (/v1/bot has no SpaceMiddleware).
func TestPrincipalCarrier_SpaceID(t *testing.T) {
	if got := (userPrincipal{spaceID: "S1"}).SpaceID(); got != "S1" {
		t.Fatalf("user SpaceID = %q, want S1", got)
	}
	if got := (userBotPrincipal{botUID: "b"}).SpaceID(); got != "" {
		t.Fatalf("user_bot SpaceID = %q, want empty", got)
	}
	if got := (oboPrincipal{grantorUID: "g", spaceID: "S1"}).SpaceID(); got != "S1" {
		t.Fatalf("obo SpaceID = %q, want S1", got)
	}
	if got := (ukPrincipal{keyUID: "k", spaceID: "S2"}).SpaceID(); got != "S2" {
		t.Fatalf("uk SpaceID = %q, want S2", got)
	}
}

// TestPrincipalCarrier_RateLimitKey — as-bot AND obo meter by botUID (防单 bot
// 打爆); uk by key UID; user by its own uid.
func TestPrincipalCarrier_RateLimitKey(t *testing.T) {
	if got := (userPrincipal{uid: "alice"}).RateLimitKey(); got != "alice" {
		t.Fatalf("user RateLimitKey = %q, want alice", got)
	}
	if got := (userBotPrincipal{botUID: "bot9"}).RateLimitKey(); got != "bot9" {
		t.Fatalf("user_bot RateLimitKey = %q, want bot9", got)
	}
	if got := (oboPrincipal{botUID: "bot9", grantorUID: "grace"}).RateLimitKey(); got != "bot9" {
		t.Fatalf("obo RateLimitKey = %q, want bot9 (meter by bot, not grantor)", got)
	}
	if got := (ukPrincipal{keyUID: "kate"}).RateLimitKey(); got != "kate" {
		t.Fatalf("uk RateLimitKey = %q, want kate", got)
	}
}

// TestPrincipalCarrier_AuditUIDs — as-user(OBO) records BOTH botUID and
// grantorUID for traceability; user_bot records only botUID; user/uk record
// neither (login_uid alone suffices).
func TestPrincipalCarrier_AuditUIDs(t *testing.T) {
	cases := []struct {
		name        string
		p           Principal
		wantBot     string
		wantGrantor string
	}{
		{"user", userPrincipal{uid: "alice"}, "", ""},
		{"user_bot", userBotPrincipal{botUID: "bot9"}, "bot9", ""},
		{"obo", oboPrincipal{botUID: "bot9", grantorUID: "grace"}, "bot9", "grace"},
		{"uk", ukPrincipal{keyUID: "kate"}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.AuditBotUID(); got != tc.wantBot {
				t.Fatalf("AuditBotUID = %q, want %q", got, tc.wantBot)
			}
			if got := tc.p.AuditGrantorUID(); got != tc.wantGrantor {
				t.Fatalf("AuditGrantorUID = %q, want %q", got, tc.wantGrantor)
			}
		})
	}
}

// ---------- blacklist policy ----------

// TestPrincipalBlacklistPolicy — 决策十 acceptance "blacklist 策略单测:
// user_bot=不查、obo/uk=真人双向（主体 uid 各异）". obo & uk share the real-user
// bidirectional gate; user_bot short-circuits.
func TestPrincipalBlacklistPolicy(t *testing.T) {
	cases := []struct {
		name string
		p    Principal
		want blacklistPolicy
	}{
		{"user", userPrincipal{uid: "alice"}, blacklistRealUserBidirectional},
		{"user_bot", userBotPrincipal{botUID: "bot9"}, blacklistNone},
		{"obo", oboPrincipal{botUID: "bot9", grantorUID: "grace"}, blacklistRealUserBidirectional},
		{"uk", ukPrincipal{keyUID: "kate"}, blacklistRealUserBidirectional},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.BlacklistPolicy(); got != tc.want {
				t.Fatalf("BlacklistPolicy = %v, want %v", got, tc.want)
			}
		})
	}
	// obo & uk resolve the SAME policy but from different subject uids.
	obo := oboPrincipal{botUID: "bot9", grantorUID: "grace"}
	uk := ukPrincipal{keyUID: "kate"}
	if obo.BlacklistPolicy() != uk.BlacklistPolicy() {
		t.Fatalf("obo and uk must share the real-user bidirectional policy")
	}
	if obo.SubjectUID() == uk.SubjectUID() {
		t.Fatalf("obo and uk must carry distinct subject uids (grantor vs key UID)")
	}
}

// ---------- Authenticate: user ----------

func TestAuthenticateUser(t *testing.T) {
	c, _ := newPrincipalCtx(t)
	c.Set("uid", "alice")
	c.Set("space_id", "  S1  ") // must be trimmed
	p := authenticateUser(c)
	if p.Kind() != principalKindUser {
		t.Fatalf("kind = %v, want user", p.Kind())
	}
	if p.SubjectUID() != "alice" {
		t.Fatalf("SubjectUID = %q, want alice", p.SubjectUID())
	}
	if p.SpaceID() != "S1" {
		t.Fatalf("SpaceID = %q, want trimmed S1", p.SpaceID())
	}
}

// ---------- Authenticate: user_bot ----------

func TestAuthenticateUserBot_OK(t *testing.T) {
	c, _ := newPrincipalCtx(t)
	c.Set(ctxKeyRobotID, "bot9")
	c.Set(ctxKeyBotKind, botKindUser)
	p, err := authenticateUserBot(c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Kind() != principalKindUserBot {
		t.Fatalf("kind = %v, want user_bot", p.Kind())
	}
	if p.SubjectUID() != "bot9" {
		t.Fatalf("SubjectUID = %q, want bot9", p.SubjectUID())
	}
	if p.SpaceID() != "" {
		t.Fatalf("SpaceID = %q, want empty (no SpaceMiddleware on /v1/bot)", p.SpaceID())
	}
	if p.BlacklistPolicy() != blacklistNone {
		t.Fatalf("user_bot must skip blacklist")
	}
}

func TestAuthenticateUserBot_AppBotDenied(t *testing.T) {
	c, _ := newPrincipalCtx(t)
	c.Set(ctxKeyRobotID, "appbot1")
	c.Set(ctxKeyBotKind, botKindApp)
	_, err := authenticateUserBot(c)
	if !errors.Is(err, errPrincipalAppBotDenied) {
		t.Fatalf("app bot must be denied, got err=%v", err)
	}
}

func TestAuthenticateUserBot_MissingRobotID(t *testing.T) {
	c, _ := newPrincipalCtx(t)
	_, err := authenticateUserBot(c)
	if !errors.Is(err, errPrincipalUnauthenticated) {
		t.Fatalf("missing robot_id must be unauthenticated, got err=%v", err)
	}
}

// ---------- Authenticate: obo ----------

func TestAuthenticateOBO_OK(t *testing.T) {
	p, err := authenticateOBO("bot9", "grace", "  S1 ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Kind() != principalKindOBO {
		t.Fatalf("kind = %v, want obo", p.Kind())
	}
	if p.SubjectUID() != "grace" {
		t.Fatalf("SubjectUID = %q, want grantor grace", p.SubjectUID())
	}
	if p.SpaceID() != "S1" {
		t.Fatalf("SpaceID = %q, want trimmed S1", p.SpaceID())
	}
	if p.RateLimitKey() != "bot9" || p.AuditBotUID() != "bot9" || p.AuditGrantorUID() != "grace" {
		t.Fatalf("obo audit/ratelimit fields wrong: rl=%q bot=%q grantor=%q",
			p.RateLimitKey(), p.AuditBotUID(), p.AuditGrantorUID())
	}
}

func TestAuthenticateOBO_MissingInputs(t *testing.T) {
	if _, err := authenticateOBO("", "grace", ""); !errors.Is(err, errPrincipalUnauthenticated) {
		t.Fatalf("empty botUID must be unauthenticated, got %v", err)
	}
	if _, err := authenticateOBO("bot9", "", ""); !errors.Is(err, errPrincipalUnauthenticated) {
		t.Fatalf("empty grantor must be unauthenticated, got %v", err)
	}
	if _, err := authenticateOBO("same", "same", ""); !errors.Is(err, errPrincipalUnauthenticated) {
		t.Fatalf("bot==grantor must fail closed, got %v", err)
	}
}

// ---------- Authenticate: uk (complete implementation, not a stub) ----------

func TestAuthenticateUK_OK(t *testing.T) {
	c, _ := newPrincipalCtx(t)
	c.Set(ctxKeyAPIKeyUID, "kate")
	c.Set(ctxKeyAPIKeySpaceID, " S2 ")
	p, err := authenticateUK(c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Kind() != principalKindUK {
		t.Fatalf("kind = %v, want uk", p.Kind())
	}
	if p.SubjectUID() != "kate" {
		t.Fatalf("SubjectUID = %q, want key UID kate", p.SubjectUID())
	}
	if p.SpaceID() != "S2" {
		t.Fatalf("SpaceID = %q, want trimmed api_key_space_id S2", p.SpaceID())
	}
	if p.RateLimitKey() != "kate" {
		t.Fatalf("uk must meter by key UID, got %q", p.RateLimitKey())
	}
	if p.BlacklistPolicy() != blacklistRealUserBidirectional {
		t.Fatalf("uk must apply real-user bidirectional blacklist")
	}
	if p.AuditBotUID() != "" {
		t.Fatalf("uk has no bot in the path, AuditBotUID must be empty, got %q", p.AuditBotUID())
	}
}

func TestAuthenticateUK_MissingKeyUID(t *testing.T) {
	c, _ := newPrincipalCtx(t)
	_, err := authenticateUK(c)
	if !errors.Is(err, errPrincipalUnauthenticated) {
		t.Fatalf("missing api_key_uid must be unauthenticated, got %v", err)
	}
}

// ---------- Handler.principal: lazy default vs explicit set ----------

// TestHandlerPrincipal_LazyUserDefault — with no explicit principal, the
// real-user carrier is constructed live from the auth context, so behaviour is
// identical to the pre-refactor c.GetLoginUID()/GetSpaceID() reads.
func TestHandlerPrincipal_LazyUserDefault(t *testing.T) {
	h := &Handler{Log: log.NewTLog("principal-test")}
	c, _ := newPrincipalCtx(t)
	c.Set("uid", "alice")
	c.Set("space_id", "S1")
	p := h.principal(c)
	if p.Kind() != principalKindUser || p.SubjectUID() != "alice" || p.SpaceID() != "S1" {
		t.Fatalf("lazy default must be user{alice,S1}, got kind=%v uid=%q space=%q",
			p.Kind(), p.SubjectUID(), p.SpaceID())
	}
}

// TestHandlerPrincipal_ExplicitSetWins — a route that setPrincipal()'d a bot
// carrier (#B) must be returned verbatim, overriding the lazy default.
func TestHandlerPrincipal_ExplicitSetWins(t *testing.T) {
	h := &Handler{Log: log.NewTLog("principal-test")}
	c, _ := newPrincipalCtx(t)
	c.Set("uid", "alice") // would-be lazy default
	setPrincipal(c, userBotPrincipal{botUID: "bot9"})
	p := h.principal(c)
	if p.Kind() != principalKindUserBot || p.SubjectUID() != "bot9" {
		t.Fatalf("explicit principal must win, got kind=%v uid=%q", p.Kind(), p.SubjectUID())
	}
}

// TestPrincipalForSubject_PrefersContext — the global enumeration seam reuses
// an explicitly-set principal, else falls back to a user carrier over the
// passed subject (preserving the direct-call semantics existing tests rely on).
func TestPrincipalForSubject_PrefersContext(t *testing.T) {
	c, _ := newPrincipalCtx(t)
	// No context principal → fall back to user{loginUID, spaceID}.
	p := principalForSubject(c, "alice", "S1")
	if p.Kind() != principalKindUser || p.SubjectUID() != "alice" || p.SpaceID() != "S1" {
		t.Fatalf("fallback must be user{alice,S1}, got kind=%v uid=%q space=%q",
			p.Kind(), p.SubjectUID(), p.SpaceID())
	}
	// Explicit principal set → returned regardless of the passed subject.
	setPrincipal(c, userBotPrincipal{botUID: "bot9"})
	p = principalForSubject(c, "alice", "S1")
	if p.Kind() != principalKindUserBot || p.SubjectUID() != "bot9" {
		t.Fatalf("context principal must win, got kind=%v uid=%q", p.Kind(), p.SubjectUID())
	}
}

// ---------- predicate dispatch (决策九 seam) ----------

// TestCanReadChannel_RealUserDelegates — for a real-user principal the
// single-channel gate must delegate to checkChannelAccess with the subject uid
// (self-DM passes without touching friend/blacklist).
func TestCanReadChannel_RealUserDelegates(t *testing.T) {
	uSvc := &stubAuthzUserSvc{}
	h := newAuthzHandlerFull(&stubAuthzGroupSvc{}, uSvc, &stubAuthzThreadSvc{})
	c, _ := newAuthzCtx(t)
	// self-DM: peer == subject, must pass without friend/blacklist lookups.
	if !h.canReadChannel(c, userPrincipal{uid: "me"}, channelTypePerson, "me") {
		t.Fatalf("real-user self-DM must pass canReadChannel")
	}
	if uSvc.friendCalls != 0 || uSvc.blCalls != 0 {
		t.Fatalf("self-DM must not consult friend/blacklist; got friend=%d bl=%d",
			uSvc.friendCalls, uSvc.blCalls)
	}
}

// TestCanReadChannel_UserBotFailsClosed — the as-bot branch is a #C/#D seam;
// until wired it must deny fail-closed with NOT_FOUND (never silently allow).
func TestCanReadChannel_UserBotFailsClosed(t *testing.T) {
	h := newAuthzHandlerFull(&stubAuthzGroupSvc{}, &stubAuthzUserSvc{}, &stubAuthzThreadSvc{})
	c, rec := newAuthzCtx(t)
	if h.canReadChannel(c, userBotPrincipal{botUID: "bot9"}, channelTypeGroup, "g1") {
		t.Fatalf("as-bot gate must fail closed until #C/#D wire it")
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("as-bot denial must render a response (NOT_FOUND)")
	}
}

// TestEnumerateReadableChannels_UserBotEnumerates — the as-bot allowlist
// enumeration is #E (YUJ-52). enumerateReadableChannels must dispatch a
// userBotPrincipal to buildBotAllowlist (friends ∪ groups ∪ threads), NOT the
// old fail-closed placeholder. Full behaviour is covered in
// search_global_test.go; here we only assert the dispatch is wired.
func TestEnumerateReadableChannels_UserBotEnumerates(t *testing.T) {
	gSvc := &stubGroupSvc{groupsByUID: map[string][]*group.InfoResp{"bot9": {{GroupNo: "g1"}}}}
	uSvc := &stubUserSvc{friends: []*user.FriendResp{{UID: "alice"}}}
	h := newHandlerForGlobalTests()
	h.groupService = gSvc
	h.userService = uSvc
	h.threadEnumFn = func([]string) (map[string][]string, error) { return nil, nil }
	c, _ := newAuthzCtx(t)
	allowGroup, allowDM, _, _, err := h.enumerateReadableChannels(c, userBotPrincipal{botUID: "bot9"})
	if err != nil {
		t.Fatalf("as-bot enumeration must succeed once #E is wired, got err=%v", err)
	}
	if len(allowGroup) != 1 || allowGroup[0].OSChannelID != "g1" {
		t.Fatalf("expected bot group g1 in allowlist, got %+v", allowGroup)
	}
	if len(allowDM) != 1 || allowDM[0].WireID != "alice" {
		t.Fatalf("expected bot friend alice in DM allowlist, got %+v", allowDM)
	}
}

// TestPrincipalKindString / blacklistPolicyString — stable labels for logs.
func TestPrincipalKindString(t *testing.T) {
	if principalKindUser.String() != "user" || principalKindUserBot.String() != "user_bot" ||
		principalKindOBO.String() != "obo" || principalKindUK.String() != "uk" {
		t.Fatalf("principalKind labels drifted")
	}
	if blacklistNone.String() != "none" || blacklistRealUserBidirectional.String() != "real-user-bidirectional" {
		t.Fatalf("blacklistPolicy labels drifted")
	}
}
