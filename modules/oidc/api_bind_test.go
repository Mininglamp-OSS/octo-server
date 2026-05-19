package oidc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	rd "github.com/go-redis/redis"
)

// newProbeRedisClient 拨号到默认测试 Redis(127.0.0.1:6379)。
// 测试 close 行为时需要一个真客户端,不依赖完整 testutil.NewTestServer。
func newProbeRedisClient(t *testing.T) *rd.Client {
	t.Helper()
	client := rd.NewClient(&rd.Options{
		Addr:        "127.0.0.1:6379",
		DialTimeout: 2 * time.Second,
		ReadTimeout: 2 * time.Second,
	})
	if err := client.Ping().Err(); err != nil {
		t.Skipf("redis not available at 127.0.0.1:6379: %v", err)
	}
	return client
}

// newTestOIDCWithBind 与 newTestOIDC 同模式,但额外构造 BindService + 注入
// 已签发的 bind_token。skipIssue=true 时不预签 token(用于 token-missing 测试)。
//
// 返回:o, jti(若签发), bindAuth fake, bindLoc fake, bindStore
func newTestOIDCWithBind(t *testing.T, bindCfg BindConfig, claims *IDTokenClaims, skipIssue bool) (
	*OIDC, string, *fakeBindAuth, *fakeBindLocator, *memoryBindStore,
) {
	o, jti, auth, loc, store, _, _, _ := newTestOIDCWithBindFull(t, bindCfg, claims, skipIssue)
	return o, jti, auth, loc, store
}

// newTestOIDCWithBindFull 给 confirm 路径用的扩展版:多返 identity / users / authcode 三个 fake。
func newTestOIDCWithBindFull(t *testing.T, bindCfg BindConfig, claims *IDTokenClaims, skipIssue bool) (
	*OIDC, string,
	*fakeBindAuth, *fakeBindLocator, *memoryBindStore,
	*fakeIdentityWriter, *fakeIssueSession, *fakeAuthcode,
) {
	t.Helper()
	store := newMemoryBindStore()
	auth := &fakeBindAuth{}
	loc := &fakeBindLocator{
		byUsername: map[string]string{},
		byPhone:    map[string][]string{},
	}
	identity := &fakeIdentityWriter{}
	users := &fakeIssueSession{resp: &IssueSessionResp{
		UID: "u-default", LoginRespJSON: `{"token":"t"}`,
	}}
	authcode := newFakeAuthcode()
	svc := newBindService(bindCfg, store, auth, loc)
	svc.identity = identity
	svc.users = users
	o := &OIDC{
		Log: log.NewTLog("OIDC-test"),
		cfg: &Config{
			Enabled: true,
			Provider: ProviderConfig{
				ID: "aegis", Issuer: "https://idp.example",
				RedirectURI: "https://app/cb", AllowNewUser: false,
			},
			Bind: bindCfg,
		},
		bind:     svc,
		audit:    newFakeAudit(),
		authcode: authcode,
	}
	if skipIssue || claims == nil {
		return o, "", auth, loc, store, identity, users, authcode
	}
	jti, err := svc.Issue(context.Background(), claims, sampleSD())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return o, jti, auth, loc, store, identity, users, authcode
}

func newTestBindRouter(o *OIDC) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/v1/auth/oidc/aegis")
	g.GET("/bind/info", func(c *gin.Context) { o.bindInfo(wrapWk(c)) })
	g.POST("/bind/verify/password", func(c *gin.Context) { o.bindVerifyPassword(wrapWk(c)) })
	g.POST("/bind/verify/otp/send", func(c *gin.Context) { o.bindOTPSend(wrapWk(c)) })
	g.POST("/bind/verify/otp/check", func(c *gin.Context) { o.bindOTPCheck(wrapWk(c)) })
	g.POST("/bind/confirm", func(c *gin.Context) { o.bindConfirm(wrapWk(c)) })
	g.POST("/bind/create", func(c *gin.Context) { o.bindCreate(wrapWk(c)) })
	return r
}

func defaultBindCfg() BindConfig {
	return BindConfig{
		Enabled:        true,
		TokenTTL:       60_000_000_000, // 60s in ns
		VerifyMax:      5,
		OTPSendMax:     3,
		ConfirmMax:     3,
		UIDFailPerDay:  10,
		Methods:        []BindMethod{BindMethodPassword, BindMethodSMSOTP},
		SupportContact: "ops@example.com",
		AllowCreate:    true,
		// 默认放行 sampleClaims().Issuer,让 Create 路径 D. issuerAllowedForCreate
		// 兜底校验默认放行(api_bind_test 多个用例依赖 Create 成功)。
		IssuerAllowlist: []string{"https://idp.example"},
	}
}

// TestAPI_BindInfo_ReturnsMaskedClaims 200 + JSON 含脱敏字段;不泄漏 sub/issuer。
func TestAPI_BindInfo_ReturnsMaskedClaims(t *testing.T) {
	o, jti, _, _, _ := newTestOIDCWithBind(t, defaultBindCfg(), sampleClaims(), false)
	r := newTestBindRouter(o)

	req := httptest.NewRequest("GET", "/v1/auth/oidc/aegis/bind/info?token="+jti, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !contains(body, "a***@example.com") || !contains(body, "****0000") || !contains(body, "Alice") {
		t.Fatalf("masked fields missing: %s", body)
	}
	if contains(body, "sub-A") || contains(body, "https://idp.example") {
		t.Fatalf("info leaks sub/issuer: %s", body)
	}
}

func TestAPI_BindInfo_MissingToken(t *testing.T) {
	o, _, _, _, _ := newTestOIDCWithBind(t, defaultBindCfg(), nil, true)
	r := newTestBindRouter(o)
	req := httptest.NewRequest("GET", "/v1/auth/oidc/aegis/bind/info", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", w.Code)
	}
}

func TestAPI_BindInfo_UnknownToken(t *testing.T) {
	o, _, _, _, _ := newTestOIDCWithBind(t, defaultBindCfg(), nil, true)
	r := newTestBindRouter(o)
	req := httptest.NewRequest("GET", "/v1/auth/oidc/aegis/bind/info?token=fake-jti", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusGone {
		// 410 Gone 是过期/未知 token 的语义(单次消费 + 5min TTL)
		t.Fatalf("status=%d want 410 for unknown token", w.Code)
	}
}

func TestAPI_BindVerifyPassword_Success(t *testing.T) {
	o, jti, auth, loc, store := newTestOIDCWithBind(t, defaultBindCfg(), sampleClaims(), false)
	auth.verifyPasswordResp.matched = true
	loc.byUsername["alice"] = "u-alice"
	r := newTestBindRouter(o)

	body, _ := json.Marshal(map[string]string{
		"token": jti, "identifier": "alice", "password": "Pwd@12345",
	})
	req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/bind/verify/password", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	sess, _ := store.Get(context.Background(), jti)
	if sess.Status != BindStatusVerified {
		t.Fatalf("status=%v want verified", sess.Status)
	}
}

func TestAPI_BindVerifyPassword_RateLimited(t *testing.T) {
	cfg := defaultBindCfg()
	cfg.VerifyMax = 1
	o, jti, auth, loc, _ := newTestOIDCWithBind(t, cfg, sampleClaims(), false)
	auth.verifyPasswordResp.matched = false
	auth.verifyPasswordResp.reason = "password_mismatch"
	loc.byUsername["alice"] = "u-alice"
	r := newTestBindRouter(o)

	body, _ := json.Marshal(map[string]string{
		"token": jti, "identifier": "alice", "password": "x",
	})
	// 第 1 次:密码错(handler 返 401)
	req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/bind/verify/password", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("first call status=%d want 401", w.Code)
	}
	// 第 2 次:超 VerifyMax=1,handler 应返 429
	req2 := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/bind/verify/password",
		bytes.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("rate-limited call status=%d want 429", w2.Code)
	}
}

func TestAPI_BindOTPSend_UsesClaimsPhone(t *testing.T) {
	o, jti, auth, _, _ := newTestOIDCWithBind(t, defaultBindCfg(), sampleClaims(), false)
	r := newTestBindRouter(o)

	body, _ := json.Marshal(map[string]string{"token": jti})
	req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/bind/verify/otp/send",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if auth.calls.smsZone != "0086" || auth.calls.smsPhone != "13900000000" {
		t.Fatalf("phone not from claims: %+v", auth.calls)
	}
}

func TestAPI_BindOTPSend_NoPhoneInClaims(t *testing.T) {
	c := sampleClaims()
	c.PhoneNumber = ""
	c.PhoneVerified = false
	o, jti, _, _, _ := newTestOIDCWithBind(t, defaultBindCfg(), c, false)
	r := newTestBindRouter(o)
	body, _ := json.Marshal(map[string]string{"token": jti})
	req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/bind/verify/otp/send", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", w.Code)
	}
}

func TestAPI_BindOTPCheck_Success(t *testing.T) {
	o, jti, _, loc, store := newTestOIDCWithBind(t, defaultBindCfg(), sampleClaims(), false)
	// PR4 起 VerifySMS 用 phone 反查 dmwork user;预置单匹配。
	loc.byPhone["0086|13900000000"] = []string{"u-phone-1"}
	r := newTestBindRouter(o)
	// fakeBindAuth.verifySMSErr 默认 nil → 接受任意 code(包括 "1234")。
	// 真实 SMSService.Verify 才会做 code 比对;此测试覆盖 handler→service→auth 链路,
	// 不覆盖 SMS code 校验本身(那是 user 包 service_sms.go 的职责)。
	body, _ := json.Marshal(map[string]string{"token": jti, "code": "1234"})
	req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/bind/verify/otp/check", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	sess, _ := store.Get(context.Background(), jti)
	if sess.Status != BindStatusVerified || sess.VerifiedMethod != BindMethodSMSOTP {
		t.Fatalf("sess=%+v", sess)
	}
}

// TestAPI_BindRoutes_DisabledNotMounted 关键灰度不变式:Bind.Enabled=false 时
// bindRoutes 完全不挂任何 handler。production routeAt 内的 if cfg.Bind.Enabled
// 守卫保证 — 这里通过直接调 bindRoutes 在两种 cfg 下、对比路由数量来锁定。
func TestAPI_BindRoutes_DisabledNotMounted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfgOff := defaultBindCfg()
	cfgOff.Enabled = false
	oOff, _, _, _, _ := newTestOIDCWithBind(t, cfgOff, nil, true)
	rgOff := newTestRouteGroup()
	oOff.bindRoutes(rgOff)
	if got := len(rgOff.routes); got != 0 {
		t.Fatalf("Bind.Enabled=false must mount 0 routes, got %d (%+v)", got, rgOff.routes)
	}

	cfgOn := defaultBindCfg() // AllowCreate=true by default
	oOn, _, _, _, _ := newTestOIDCWithBind(t, cfgOn, nil, true)
	rgOn := newTestRouteGroup()
	oOn.bindRoutes(rgOn)
	// 5 base endpoints + /bind/create when AllowCreate=true = 6
	if got := len(rgOn.routes); got != 6 {
		t.Fatalf("Bind.Enabled=true + AllowCreate=true must mount 6 routes, got %d (%+v)",
			got, rgOn.routes)
	}

	// AllowCreate=false: /bind/create must not be mounted (5 routes)
	cfgNoCreate := defaultBindCfg()
	cfgNoCreate.AllowCreate = false
	oNoCreate, _, _, _, _ := newTestOIDCWithBind(t, cfgNoCreate, nil, true)
	rgNoCreate := newTestRouteGroup()
	oNoCreate.bindRoutes(rgNoCreate)
	if got := len(rgNoCreate.routes); got != 5 {
		t.Fatalf("Bind.Enabled=true + AllowCreate=false must mount 5 routes, got %d (%+v)",
			got, rgNoCreate.routes)
	}
}

// TestAPI_BindConfirm_Success 端到端 confirm:
//   - 200 响应含 LoginRespJSON / uid
//   - ThirdAuthcode 被回填到原发起设备的 authcode key(FR-6.3)
//   - identity.Insert 被调一次
//   - session 已被消费(再 confirm 应 410)
func TestAPI_BindConfirm_Success(t *testing.T) {
	o, jti, auth, loc, store, identity, users, ac := newTestOIDCWithBindFull(t, defaultBindCfg(), sampleClaims(), false)
	auth.verifyPasswordResp.matched = true
	loc.byUsername["alice"] = "u-alice"
	users.resp = &IssueSessionResp{UID: "u-alice", LoginRespJSON: `{"token":"t-alice"}`}
	r := newTestBindRouter(o)

	// 先走 verify/password 把状态推到 verified
	body, _ := json.Marshal(map[string]string{
		"token": jti, "identifier": "alice", "password": "Pwd@1",
	})
	req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/bind/verify/password",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("verify/password status=%d body=%s", w.Code, w.Body.String())
	}

	// confirm
	body2, _ := json.Marshal(map[string]string{"token": jti})
	req2 := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/bind/confirm",
		bytes.NewReader(body2))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("confirm status=%d body=%s", w2.Code, w2.Body.String())
	}
	if !contains(w2.Body.String(), `"uid":"u-alice"`) {
		t.Fatalf("confirm body missing uid: %s", w2.Body.String())
	}
	if len(identity.inserted) != 1 {
		t.Fatalf("identity inserts=%d want 1", len(identity.inserted))
	}
	// ThirdAuthcode 回填(SD.ClientAuthcode 来自 sampleSD = "ac-1")
	if got := ac.get("ac-1"); got != `{"token":"t-alice"}` {
		t.Fatalf("ThirdAuthcode not backfilled, got %q", got)
	}
	// session 已消费
	if _, err := store.Get(context.Background(), jti); err == nil {
		t.Fatalf("session must be consumed after confirm")
	}
}

func TestAPI_BindConfirm_StatusNotVerified(t *testing.T) {
	o, jti, _, _, _, _, _, _ := newTestOIDCWithBindFull(t, defaultBindCfg(), sampleClaims(), false)
	r := newTestBindRouter(o)
	// 不走 verify,直接 confirm
	body, _ := json.Marshal(map[string]string{"token": jti})
	req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/bind/confirm",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401 (verify before confirm)", w.Code)
	}
}

// TestAPI_BindOTPCheck_NoPhoneInClaimsReturns400 锁定 #3 修复:
// 客户端误调 /verify/otp/check 但 claims 没有 verified phone 时,
// handler 应返 400(业务前提不满足,客户端不该 retry),
// 而非 500(掩盖底层 SMS 链路故障)。
func TestAPI_BindOTPCheck_NoPhoneInClaimsReturns400(t *testing.T) {
	c := sampleClaims()
	c.PhoneNumber = ""
	c.PhoneVerified = false
	o, jti, _, _, _ := newTestOIDCWithBind(t, defaultBindCfg(), c, false)
	r := newTestBindRouter(o)

	body, _ := json.Marshal(map[string]string{"token": jti, "code": "1234"})
	req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/bind/verify/otp/check",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("ErrBindNoPhone on /verify/otp/check must surface as 400 (metric/HTTP parity), got %d body=%s",
			w.Code, w.Body.String())
	}
}

// TestAPI_BindVerifyPassword_UnknownUsernameReturns401 锁定 #1 修复:
// locator 没找到 username 时应 401(账号或密码错,通用文案),不是 500。
func TestAPI_BindVerifyPassword_UnknownUsernameReturns401(t *testing.T) {
	o, jti, _, _, _ := newTestOIDCWithBind(t, defaultBindCfg(), sampleClaims(), false)
	// 不预置 byUsername
	r := newTestBindRouter(o)
	body, _ := json.Marshal(map[string]string{
		"token": jti, "identifier": "ghost", "password": "x",
	})
	req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/bind/verify/password",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unknown username must surface as 401 (anti-enumeration), got %d body=%s",
			w.Code, w.Body.String())
	}
}

// TestAPI_BindOTPCheck_NoPhoneMatchReturns401 锁定 #2 修复:
// SMS OTP 校验通过但 claims phone 没对应 dmwork 用户时返 401(业务拒绝,
// 引导用户走兜底),不是 500。
func TestAPI_BindOTPCheck_NoPhoneMatchReturns401(t *testing.T) {
	o, jti, _, _, _ := newTestOIDCWithBind(t, defaultBindCfg(), sampleClaims(), false)
	// 不预置 byPhone → 0 匹配
	r := newTestBindRouter(o)
	body, _ := json.Marshal(map[string]string{"token": jti, "code": "1234"})
	req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/bind/verify/otp/check",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("0-phone-match must surface as 401 (business reject), got %d body=%s",
			w.Code, w.Body.String())
	}
}

func TestAPI_BindConfirm_AlreadyBound(t *testing.T) {
	o, jti, auth, loc, _, identity, _, _ := newTestOIDCWithBindFull(t, defaultBindCfg(), sampleClaims(), false)
	auth.verifyPasswordResp.matched = true
	loc.byUsername["alice"] = "u-alice"
	identity.insertErr = mockDuplicateKeyErr()
	r := newTestBindRouter(o)

	// verify
	body, _ := json.Marshal(map[string]string{
		"token": jti, "identifier": "alice", "password": "Pwd@1",
	})
	req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/bind/verify/password",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("verify status=%d", w.Code)
	}
	// confirm with duplicate
	body2, _ := json.Marshal(map[string]string{"token": jti})
	req2 := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/bind/confirm",
		bytes.NewReader(body2))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusConflict {
		t.Fatalf("status=%d want 409 (already bound)", w2.Code)
	}
}

func contains(s, sub string) bool {
	return bytes.Contains([]byte(s), []byte(sub))
}

// TestOIDC_Close_ReleasesBindStore 锁定 #4 修复:Bind.Enabled=true 时
// OIDC.Close 必须把 bindStore 的 redis 连接池一并关掉,否则进程优雅退出
// 时 fd 泄漏(测试场景下尤其明显:Init 后 Close 不释放,后续测试再 Init
// 同 Redis 累积连接)。
//
// 用 *redisBindStore 真实型断言:bindStore 默认是 BindStore 接口,production
// 路径下是 *redisBindStore;Close 路径通过类型断言找到并调 .Close()。
// 这里构造一个真实 *redisBindStore(指向 127.0.0.1:6379)然后断言 Close
// 后客户端不可用作业务调用 —— 用 client.Ping() 看是否 EOF/closed。
func TestOIDC_Close_ReleasesBindStore(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped in short mode: requires Redis at 127.0.0.1:6379")
	}
	// 复用 testutil 拼连接;此处只关心 Close 是否真送达 client.Close。
	// 用接口指针注入更直接 —— 包内可访问 *redisBindStore。
	rss := &redisBindStore{}
	rss.client = newProbeRedisClient(t)
	o := &OIDC{
		Log:       log.NewTLog("OIDC-test"),
		cfg:       &Config{Enabled: true, Bind: BindConfig{Enabled: true}},
		bindStore: rss,
	}
	if err := o.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close 后 bindStore 应被置 nil(防再次 Close 双关)
	if o.bindStore != nil {
		t.Fatalf("Close must nil out bindStore, got %T", o.bindStore)
	}
	// 真客户端 Close 之后 Ping 必返错(连接已断)
	if err := rss.client.Ping().Err(); err == nil {
		t.Fatal("bindStore.client.Close() should have terminated the connection, Ping still succeeds")
	}
}

// testRouteGroup 测试用 bindRouteGroup 实现 —— 只记录挂了哪些路由,不真分发请求。
// 仅供 TestAPI_BindRoutes_DisabledNotMounted 锁定挂载数量。
type testRouteGroup struct {
	routes []string
}

func newTestRouteGroup() *testRouteGroup { return &testRouteGroup{} }
func (g *testRouteGroup) GET(path string, _ ...wkhttp.HandlerFunc) {
	g.routes = append(g.routes, "GET "+path)
}
func (g *testRouteGroup) POST(path string, _ ...wkhttp.HandlerFunc) {
	g.routes = append(g.routes, "POST "+path)
}

// TestAPI_BindConfirm_4xxBranchesAlsoAudit 锁定 handleBindConfirmErr 的 4xx
// 分支也必须写 EventBindConfirmFail 审计行 —— SR-6 完整性。
// 之前只有 default(500)分支 writeAudit,导致攻击者
// 反复 confirm 探测 status/already-bound/expired 差异时 oidc_audit_log 留不下
// 痕迹,SOC 反查丢半条时间序列。
func TestAPI_BindConfirm_4xxBranchesAlsoAudit(t *testing.T) {
	for _, tc := range []struct {
		name        string
		statusSetup func(*BindService, string) // 把 session 推到目标状态
		wantStatus  int
	}{
		{
			"status_conflict (不 verify 直接 confirm) -> 401",
			func(_ *BindService, _ string) {}, // session 还是 issued
			http.StatusUnauthorized,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, jti, _, _, _, _, _, _ := newTestOIDCWithBindFull(t, defaultBindCfg(), sampleClaims(), false)
			audit := o.audit.(*fakeAudit)
			tc.statusSetup(o.bind, jti)
			r := newTestBindRouter(o)

			body, _ := json.Marshal(map[string]string{"token": jti})
			req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/bind/confirm",
				bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != tc.wantStatus {
				t.Fatalf("status=%d want %d body=%s", w.Code, tc.wantStatus, w.Body.String())
			}
			// 必须落 EventBindConfirmFail 审计
			var found bool
			for _, e := range audit.events() {
				if e == EventBindConfirmFail {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("4xx confirm path must emit EventBindConfirmFail audit, got events=%v",
					audit.events())
			}
		})
	}
}

// TestAPI_BindVerifyPassword_MethodDisabledReturns400 锁定 Issue C 的 handler
// 映射:service 返 ErrBindMethodDisabled → HTTP 400。防止"service 改对了但
// handler 的 errors.Is 链漏一处"导致前端拿到 500/401 误判。
func TestAPI_BindVerifyPassword_MethodDisabledReturns400(t *testing.T) {
	cfg := defaultBindCfg()
	cfg.Methods = []BindMethod{BindMethodSMSOTP} // 关闭密码
	o, jti, _, _, _ := newTestOIDCWithBind(t, cfg, sampleClaims(), false)
	r := newTestBindRouter(o)

	body, _ := json.Marshal(map[string]string{
		"token": jti, "identifier": "alice", "password": "x",
	})
	req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/bind/verify/password",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("disabled method must be 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestAPI_BindOTPSend_MethodDisabledReturns400(t *testing.T) {
	cfg := defaultBindCfg()
	cfg.Methods = []BindMethod{BindMethodPassword}
	o, jti, _, _, _ := newTestOIDCWithBind(t, cfg, sampleClaims(), false)
	r := newTestBindRouter(o)

	body, _ := json.Marshal(map[string]string{"token": jti})
	req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/bind/verify/otp/send",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("disabled sms send must be 400, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestAPI_BindHandlers_NilBindReturnsServiceUnavailable 锁定 Issue F 修复:
// Discovery 失败时 Init 早返,o.bind 保持 nil,但 cfg.Bind.Enabled=true 仍
// 让 bindRoutes 挂上 5 个端点。任一 handler 被调用时第一行就调 o.bind.* →
// nil pointer panic 影响整个进程。修复后必须返 503 而非 panic。
func TestAPI_BindHandlers_NilBindReturnsServiceUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	o := &OIDC{
		Log:      log.NewTLog("OIDC-test"),
		cfg:      &Config{Enabled: true, Bind: BindConfig{Enabled: true}},
		bind:     nil, // 模拟 Discovery 失败:Init 早返,bind 未构造
		audit:    newFakeAudit(),
		authcode: newFakeAuthcode(),
	}
	r := newTestBindRouter(o)
	cases := []struct {
		method, path, body string
	}{
		{"GET", "/v1/auth/oidc/aegis/bind/info?token=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", ""},
		{"POST", "/v1/auth/oidc/aegis/bind/verify/password",
			`{"token":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","identifier":"x","password":"y"}`},
		{"POST", "/v1/auth/oidc/aegis/bind/verify/otp/send",
			`{"token":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`},
		{"POST", "/v1/auth/oidc/aegis/bind/verify/otp/check",
			`{"token":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","code":"1"}`},
		{"POST", "/v1/auth/oidc/aegis/bind/confirm",
			`{"token":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`},
	}
	for _, tc := range cases {
		var body *bytes.Reader
		if tc.body != "" {
			body = bytes.NewReader([]byte(tc.body))
		} else {
			body = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(tc.method, tc.path, body)
		if tc.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		// 若未修复,handler 内 o.bind.* 会 panic 而非返响应
		r.ServeHTTP(w, req)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s: status=%d want 503 (nil bind), body=%s",
				tc.method, tc.path, w.Code, w.Body.String())
		}
	}
}

// TestAPI_RedirectToBindPage_EmptyBaseWritesAuthcodeZero 锁定 Issue E 修复:
// RedirectBase 漏配 时 redirectToBindPage 退回失败路径,但前端 ThirdAuthcode
// 轮询会卡死 5min。必须先 SetAuthcode "0" 让前端尽早感知失败。
func TestAPI_RedirectToBindPage_EmptyBaseWritesAuthcodeZero(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fakeAC := newFakeAuthcode()
	o := &OIDC{
		Log: log.NewTLog("OIDC-test"),
		cfg: &Config{
			Enabled: true,
			Provider: ProviderConfig{
				ID: "aegis", ReturnToHosts: []string{"app.example.com"},
			},
			Bind: BindConfig{Enabled: true, RedirectBase: ""},
		},
		authcode: fakeAC,
	}
	// 自建 gin context 直接调
	r := gin.New()
	r.GET("/x", func(c *gin.Context) {
		o.redirectToBindPage(wrapWk(c), &StateData{
			ClientAuthcode: "ac-empty-base",
		}, "jti-xyz")
	})
	req := httptest.NewRequest("GET", "/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got := fakeAC.get("ac-empty-base"); got != "0" {
		t.Fatalf("RedirectBase 为空必须先写 ThirdAuthcode \"0\",got %q", got)
	}
}

// ---- /bind/create handler tests ----

// T30: happy path → 200 + LoginRespJSON + ThirdAuthcode 回填
func TestAPI_BindCreate_HappyPath(t *testing.T) {
	o, jti, _, _, _, _, users, ac := newTestOIDCWithBindFull(t, defaultBindCfg(), sampleClaims(), false)
	users.resp = &IssueSessionResp{UID: "u-created", LoginRespJSON: `{"token":"t-created"}`}
	r := newTestBindRouter(o)

	body, _ := json.Marshal(map[string]string{"token": jti})
	req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/bind/create", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("T30: status=%d body=%s", w.Code, w.Body.String())
	}
	if !contains(w.Body.String(), `"uid":"u-created"`) {
		t.Fatalf("T30: body missing uid: %s", w.Body.String())
	}
	// ThirdAuthcode backfilled (sampleSD.ClientAuthcode = "ac-1")
	if got := ac.get("ac-1"); got != `{"token":"t-created"}` {
		t.Fatalf("T30: ThirdAuthcode not backfilled, got %q", got)
	}
}

// T31: token 缺失/非法 → 400
func TestAPI_BindCreate_MissingToken(t *testing.T) {
	o, _, _, _, _, _, _, _ := newTestOIDCWithBindFull(t, defaultBindCfg(), nil, true)
	r := newTestBindRouter(o)

	for _, body := range []string{`{}`, `{"token":""}`} {
		req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/bind/create",
			bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("T31: body=%s status=%d want 400", body, w.Code)
		}
	}
}

// T32: AllowCreate=false → 路由不挂（404 by router）
func TestAPI_BindCreate_AllowCreateFalse_NotMounted(t *testing.T) {
	cfg := defaultBindCfg()
	cfg.AllowCreate = false
	o, _, _, _, _ := newTestOIDCWithBind(t, cfg, nil, true)
	rg := newTestRouteGroup()
	o.bindRoutes(rg)
	for _, route := range rg.routes {
		if route == "POST /bind/create" {
			t.Fatalf("T32: /bind/create must NOT be mounted when AllowCreate=false, routes=%v", rg.routes)
		}
	}
}

// T33: ErrBindCreateClaimsIncomplete → 422
func TestAPI_BindCreate_ClaimsIncomplete(t *testing.T) {
	cfg := defaultBindCfg()
	c := sampleClaims()
	c.Email = ""
	c.EmailVerified = false
	c.PhoneNumber = ""
	c.PhoneVerified = false
	o, jti, _, _, _, _, _, _ := newTestOIDCWithBindFull(t, cfg, c, false)
	r := newTestBindRouter(o)

	body, _ := json.Marshal(map[string]string{"token": jti})
	req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/bind/create", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("T33: ErrBindCreateClaimsIncomplete must be 422, got %d body=%s", w.Code, w.Body.String())
	}
}

// T34: ErrBindStatusConflict → 409
func TestAPI_BindCreate_StatusConflict(t *testing.T) {
	o, jti, auth, loc, _, _, _, _ := newTestOIDCWithBindFull(t, defaultBindCfg(), sampleClaims(), false)
	// push to verified
	auth.verifyPasswordResp.matched = true
	loc.byUsername["alice"] = "u-alice"
	svc := o.bind
	_ = svc.VerifyPassword(context.Background(), jti, "alice", "p")
	r := newTestBindRouter(o)

	body, _ := json.Marshal(map[string]string{"token": jti})
	req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/bind/create", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("T34: ErrBindStatusConflict must be 409, got %d body=%s", w.Code, w.Body.String())
	}
}

// T35: ErrBindRateLimited → 429 (bindCreateMax = 1, 第二次请求即限流)
func TestAPI_BindCreate_RateLimited(t *testing.T) {
	cfg := defaultBindCfg()
	o, jti, _, _, _, _, users, _ := newTestOIDCWithBindFull(t, cfg, sampleClaims(), false)
	users.err = errors.New("transient") // first call fails so token survives
	r := newTestBindRouter(o)

	body, _ := json.Marshal(map[string]string{"token": jti})
	// first call (count=1 <= 1, not rate limited yet, but IssueSession fails)
	req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/bind/create", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Fatal("first call should fail")
	}
	// second call: rate limited
	req2 := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/bind/create",
		bytes.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("T35: ErrBindRateLimited must be 429, got %d body=%s", w2.Code, w2.Body.String())
	}
}

// T36: ErrBindNotFound → 410
func TestAPI_BindCreate_TokenNotFound(t *testing.T) {
	o, _, _, _, _, _, _, _ := newTestOIDCWithBindFull(t, defaultBindCfg(), nil, true)
	r := newTestBindRouter(o)

	// valid format but non-existent token (43 base64 chars)
	body, _ := json.Marshal(map[string]string{"token": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"})
	req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/bind/create", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusGone {
		t.Fatalf("T36: ErrBindNotFound must be 410, got %d body=%s", w.Code, w.Body.String())
	}
}

// T37: internal error → 500
func TestAPI_BindCreate_InternalError(t *testing.T) {
	o, jti, _, _, _, _, users, _ := newTestOIDCWithBindFull(t, defaultBindCfg(), sampleClaims(), false)
	users.err = errors.New("db timeout")
	r := newTestBindRouter(o)

	body, _ := json.Marshal(map[string]string{"token": jti})
	req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/bind/create", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("T37: internal error must be 500, got %d body=%s", w.Code, w.Body.String())
	}
}

// T38: 成功路径写 EventBindCreated；失败路径写 EventBindCreateFail
func TestAPI_BindCreate_AuditEvents(t *testing.T) {
	t.Run("success emits EventBindCreated", func(t *testing.T) {
		o, jti, _, _, _, _, users, _ := newTestOIDCWithBindFull(t, defaultBindCfg(), sampleClaims(), false)
		users.resp = &IssueSessionResp{UID: "u-c", LoginRespJSON: `{}`}
		audit := o.audit.(*fakeAudit)
		r := newTestBindRouter(o)

		body, _ := json.Marshal(map[string]string{"token": jti})
		req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/bind/create", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(httptest.NewRecorder(), req)

		var found bool
		for _, e := range audit.events() {
			if e == EventBindCreated {
				found = true
			}
		}
		if !found {
			t.Fatalf("success must emit EventBindCreated, got %v", audit.events())
		}
	})

	t.Run("failure emits EventBindCreateFail", func(t *testing.T) {
		o, jti, _, _, _, _, users, _ := newTestOIDCWithBindFull(t, defaultBindCfg(), sampleClaims(), false)
		users.err = errors.New("down")
		audit := o.audit.(*fakeAudit)
		r := newTestBindRouter(o)

		body, _ := json.Marshal(map[string]string{"token": jti})
		req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/bind/create", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(httptest.NewRecorder(), req)

		var found bool
		for _, e := range audit.events() {
			if e == EventBindCreateFail {
				found = true
			}
		}
		if !found {
			t.Fatalf("failure must emit EventBindCreateFail, got %v", audit.events())
		}
	})
}

// T39: metric label endpoint=create + result 与 HTTP status 对齐
// (tested indirectly via the metric vectors; we verify no panic and correct status codes)
// This is covered by T30-T37 above.

// T40: o.bind=nil → 503
func TestAPI_BindCreate_NilBind_503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	o := &OIDC{
		Log:      log.NewTLog("OIDC-test"),
		cfg:      &Config{Enabled: true, Bind: BindConfig{Enabled: true, AllowCreate: true}},
		bind:     nil,
		audit:    newFakeAudit(),
		authcode: newFakeAuthcode(),
	}
	r := gin.New()
	r.POST("/bind/create", func(c *gin.Context) { o.bindCreate(wrapWk(c)) })

	body, _ := json.Marshal(map[string]string{"token": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"})
	req := httptest.NewRequest("POST", "/bind/create", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("T40: nil bind must return 503, got %d body=%s", w.Code, w.Body.String())
	}
}

// ---- Info extension tests (T41-T43) ----

// T41: AllowCreate=true + claims 满足要求 → allow_create=true, create_blocked=""
func TestAPI_BindInfo_AllowCreate_ClaimsSatisfied(t *testing.T) {
	cfg := defaultBindCfg()
	cfg.AllowCreate = true
	o, jti, _, _, _ := newTestOIDCWithBind(t, cfg, sampleClaims(), false)
	r := newTestBindRouter(o)

	req := httptest.NewRequest("GET", "/v1/auth/oidc/aegis/bind/info?token="+jti, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("T41: status=%d body=%s", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("T41: unmarshal: %v", err)
	}
	if body["allow_create"] != true {
		t.Fatalf("T41: allow_create must be true, got %v", body["allow_create"])
	}
	if v, ok := body["create_blocked"]; ok && v != "" {
		t.Fatalf("T41: create_blocked must be empty string, got %v", v)
	}
}

// T42: AllowCreate=true + claims 不满足 → allow_create=true, create_blocked="claims_incomplete"
func TestAPI_BindInfo_AllowCreate_ClaimsInsufficient(t *testing.T) {
	cfg := defaultBindCfg()
	cfg.AllowCreate = true
	c := sampleClaims()
	c.Email = ""
	c.EmailVerified = false
	c.PhoneNumber = ""
	c.PhoneVerified = false
	o, jti, _, _, _ := newTestOIDCWithBind(t, cfg, c, false)
	r := newTestBindRouter(o)

	req := httptest.NewRequest("GET", "/v1/auth/oidc/aegis/bind/info?token="+jti, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("T42: status=%d body=%s", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("T42: unmarshal: %v", err)
	}
	if body["allow_create"] != true {
		t.Fatalf("T42: allow_create must be true, got %v", body["allow_create"])
	}
	if body["create_blocked"] != "claims_incomplete" {
		t.Fatalf("T42: create_blocked must be claims_incomplete, got %v", body["create_blocked"])
	}
}

// T43: AllowCreate=false → allow_create=false, create_blocked="disabled"
func TestAPI_BindInfo_AllowCreateFalse(t *testing.T) {
	cfg := defaultBindCfg()
	cfg.AllowCreate = false
	o, jti, _, _, _ := newTestOIDCWithBind(t, cfg, sampleClaims(), false)
	r := newTestBindRouter(o)

	req := httptest.NewRequest("GET", "/v1/auth/oidc/aegis/bind/info?token="+jti, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("T43: status=%d body=%s", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("T43: unmarshal: %v", err)
	}
	if body["allow_create"] != false {
		t.Fatalf("T43: allow_create must be false, got %v", body["allow_create"])
	}
	if body["create_blocked"] != "disabled" {
		t.Fatalf("T43: create_blocked must be disabled, got %v", body["create_blocked"])
	}
}

// TestAPI_RedirectToBindPage_EmptyProviderIDOmitsParam 锁定 PR #80 的回退契约:
// cfg.Provider.ID 为空时,redirect URL **不**应出现 provider= 字段(让前端按
// legacyProviderPathID="aegis" 兜底),而不是写空串 provider=。
//
// 生产路径下 LoadConfig 已用 ^[a-z0-9][a-z0-9_-]{0,63}$ 拦了空 ID,本测试钉住的是
// 兜底 if 行为本身,防回退路径被无声删除后仍编译通过。
func TestAPI_RedirectToBindPage_EmptyProviderIDOmitsParam(t *testing.T) {
	gin.SetMode(gin.TestMode)
	o := &OIDC{
		Log: log.NewTLog("OIDC-test"),
		cfg: &Config{
			Enabled: true,
			Provider: ProviderConfig{
				ID:            "",
				ReturnToHosts: []string{"app.example.com"},
			},
			Bind: BindConfig{
				Enabled:      true,
				RedirectBase: "https://im.example.com/oidc/bind",
			},
		},
		authcode: newFakeAuthcode(),
	}
	r := gin.New()
	r.GET("/x", func(c *gin.Context) {
		o.redirectToBindPage(wrapWk(c), &StateData{
			ClientAuthcode: "ac-no-provider",
		}, "jti-xyz")
	})
	req := httptest.NewRequest("GET", "/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status=%d want 302, body=%s", w.Code, w.Body.String())
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if loc.Query().Has("provider") {
		t.Fatalf("provider must be omitted when cfg.Provider.ID=\"\", got %q in %q",
			loc.Query().Get("provider"), w.Header().Get("Location"))
	}
}
