package oidc

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
)

// newTestOIDCWithBind 与 newTestOIDC 同模式,但额外构造 BindService + 注入
// 已签发的 bind_token。skipIssue=true 时不预签 token(用于 token-missing 测试)。
//
// 返回:o, jti(若签发), bindAuth fake, bindLoc fake, bindStore
func newTestOIDCWithBind(t *testing.T, bindCfg BindConfig, claims *IDTokenClaims, skipIssue bool) (
	*OIDC, string, *fakeBindAuth, *fakeBindLocator, *memoryBindStore,
) {
	t.Helper()
	store := newMemoryBindStore()
	auth := &fakeBindAuth{}
	loc := &fakeBindLocator{byUsername: map[string]string{}}
	svc := newBindService(bindCfg, store, auth, loc)
	o := &OIDC{
		Log: log.NewTLog("OIDC-test"),
		cfg: &Config{
			Enabled: true,
			Provider: ProviderConfig{
				ID: "aegis", Issuer: "https://idp.example",
				RedirectURI: "https://app/cb", AllowNewUser: true,
			},
			Bind: bindCfg,
		},
		bind:  svc,
		audit: newFakeAudit(),
	}
	if skipIssue || claims == nil {
		return o, "", auth, loc, store
	}
	jti, err := svc.Issue(context.Background(), claims, sampleSD())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return o, jti, auth, loc, store
}

func newTestBindRouter(o *OIDC) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/v1/auth/oidc/aegis")
	g.GET("/bind/info", func(c *gin.Context) { o.bindInfo(wrapWk(c)) })
	g.POST("/bind/verify/password", func(c *gin.Context) { o.bindVerifyPassword(wrapWk(c)) })
	g.POST("/bind/verify/otp/send", func(c *gin.Context) { o.bindOTPSend(wrapWk(c)) })
	g.POST("/bind/verify/otp/check", func(c *gin.Context) { o.bindOTPCheck(wrapWk(c)) })
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
	o, jti, _, _, store := newTestOIDCWithBind(t, defaultBindCfg(), sampleClaims(), false)
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

	cfgOn := defaultBindCfg()
	oOn, _, _, _, _ := newTestOIDCWithBind(t, cfgOn, nil, true)
	rgOn := newTestRouteGroup()
	oOn.bindRoutes(rgOn)
	if got := len(rgOn.routes); got != 4 {
		t.Fatalf("Bind.Enabled=true must mount 4 routes (info + verify/pwd + otp send + otp check), got %d (%+v)",
			got, rgOn.routes)
	}
}

func contains(s, sub string) bool {
	return bytes.Contains([]byte(s), []byte(sub))
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
