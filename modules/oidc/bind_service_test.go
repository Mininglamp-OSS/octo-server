package oidc

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ---- fakes ----

type fakeBindAuth struct {
	verifyPasswordResp struct {
		matched bool
		reason  string
		err     error
	}
	sendSMSErr   error
	verifySMSErr error
	calls        struct {
		pwdUID, pwdPassword string
		smsZone, smsPhone   string
		verifyZone, verifyPhone, verifyCode string
		pwdCount, sendCount, verifyCount    int
	}
}

func (f *fakeBindAuth) VerifyPasswordByUID(_ context.Context, uid, password string) (bool, string, error) {
	f.calls.pwdCount++
	f.calls.pwdUID = uid
	f.calls.pwdPassword = password
	return f.verifyPasswordResp.matched, f.verifyPasswordResp.reason, f.verifyPasswordResp.err
}
func (f *fakeBindAuth) SendOIDCBindSMS(_ context.Context, zone, phone string) error {
	f.calls.sendCount++
	f.calls.smsZone = zone
	f.calls.smsPhone = phone
	return f.sendSMSErr
}
func (f *fakeBindAuth) VerifyOIDCBindSMS(_ context.Context, zone, phone, code string) error {
	f.calls.verifyCount++
	f.calls.verifyZone = zone
	f.calls.verifyPhone = phone
	f.calls.verifyCode = code
	return f.verifySMSErr
}

type fakeBindLocator struct {
	byUsername map[string]string
	err        error
}

func (f *fakeBindLocator) UIDByUsername(username string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.byUsername[username], nil
}

// ---- helpers ----

func newTestBindService(t *testing.T, cfgMutators ...func(*BindConfig)) (*BindService, *memoryBindStore, *fakeBindAuth, *fakeBindLocator) {
	t.Helper()
	store := newMemoryBindStore()
	auth := &fakeBindAuth{}
	loc := &fakeBindLocator{byUsername: map[string]string{}}
	cfg := BindConfig{
		Enabled:        true,
		TokenTTL:       time.Minute,
		VerifyMax:      5,
		OTPSendMax:     3,
		ConfirmMax:     3,
		UIDFailPerDay:  10,
		Methods:        []BindMethod{BindMethodPassword, BindMethodSMSOTP},
		SupportContact: "support@example.com",
	}
	for _, mut := range cfgMutators {
		mut(&cfg)
	}
	svc := newBindService(cfg, store, auth, loc)
	return svc, store, auth, loc
}

func sampleClaims() *IDTokenClaims {
	return &IDTokenClaims{
		Issuer: "https://idp.example", Subject: "sub-A",
		Email: "alice@example.com", EmailVerified: true,
		PhoneNumber: "+8613900000000", PhoneVerified: true,
		Name: "Alice",
	}
}

func sampleSD() *StateData {
	return &StateData{
		Provider: "oidc", CodeVerifier: "cv", Nonce: "n",
		ClientAuthcode: "ac-1", IP: "1.2.3.4", UserAgent: "ua-x",
		DeviceFlag: 0,
	}
}

// ---- tests ----

// TestBindService_Issue_PersistsClaimsAndSD 锁定:
//   - 返回非空 jti,且每次都不重复
//   - Store 里能 Get 到带 Status=issued 的 session
//   - ClaimsSnapshot / SDSnapshot 完整可解回
//   - OriginIP / OriginUA 来自 SD,审计需要
func TestBindService_Issue_PersistsClaimsAndSD(t *testing.T) {
	svc, store, _, _ := newTestBindService(t)
	jti, err := svc.Issue(context.Background(), sampleClaims(), sampleSD())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if jti == "" {
		t.Fatal("Issue returned empty jti")
	}

	jti2, err := svc.Issue(context.Background(), sampleClaims(), sampleSD())
	if err != nil {
		t.Fatalf("Issue 2nd: %v", err)
	}
	if jti2 == jti {
		t.Fatal("Issue must return unique jti on each call")
	}

	sess, err := store.Get(context.Background(), jti)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if sess.Status != BindStatusIssued {
		t.Fatalf("status=%v want issued", sess.Status)
	}
	if sess.Issuer != "https://idp.example" || sess.Subject != "sub-A" {
		t.Fatalf("identity not persisted: %+v", sess)
	}
	if len(sess.ClaimsSnapshot) == 0 || len(sess.SDSnapshot) == 0 {
		t.Fatal("claims/sd snapshots must be persisted")
	}
	if sess.OriginIP != "1.2.3.4" || sess.OriginUA != "ua-x" {
		t.Fatalf("origin info not captured: %+v", sess)
	}
}

func TestBindService_Issue_NilClaimsRejected(t *testing.T) {
	svc, _, _, _ := newTestBindService(t)
	if _, err := svc.Issue(context.Background(), nil, sampleSD()); err == nil {
		t.Fatal("nil claims must be rejected (caller bug)")
	}
	c := sampleClaims()
	c.Issuer = ""
	if _, err := svc.Issue(context.Background(), c, sampleSD()); err == nil {
		t.Fatal("empty issuer must be rejected")
	}
}

// TestBindService_Info_MasksIdentity FR-2 脱敏要求:邮箱前 1 位 + ***,
// 手机后 4 位,姓名直出。完整 sub / issuer 不返回(SR-7)。
func TestBindService_Info_MasksIdentity(t *testing.T) {
	svc, _, _, _ := newTestBindService(t)
	jti, err := svc.Issue(context.Background(), sampleClaims(), sampleSD())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	info, err := svc.Info(context.Background(), jti)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.MaskedEmail != "a***@example.com" {
		t.Fatalf("MaskedEmail=%q", info.MaskedEmail)
	}
	if info.MaskedPhone != "****0000" {
		t.Fatalf("MaskedPhone=%q", info.MaskedPhone)
	}
	if info.Name != "Alice" {
		t.Fatalf("Name=%q", info.Name)
	}
	if info.SupportContact != "support@example.com" {
		t.Fatalf("SupportContact=%q", info.SupportContact)
	}
	if len(info.Methods) != 2 || info.Methods[0] != BindMethodPassword {
		t.Fatalf("Methods=%v", info.Methods)
	}
	// 检测无 sub/issuer 泄漏(以字面搜)
	if got := info.MaskedEmail + info.MaskedPhone + info.Name; got == "" {
		t.Fatal("info empty?")
	}
}

// TestBindService_Info_NoSMSMethodWhenPhoneMissing FR-3.3 推论:claims 无
// phone(或 phone_verified=false)时,前端不应当看到 sms_otp 选项。
func TestBindService_Info_NoSMSMethodWhenPhoneMissing(t *testing.T) {
	svc, _, _, _ := newTestBindService(t)
	c := sampleClaims()
	c.PhoneNumber = ""
	c.PhoneVerified = false
	jti, err := svc.Issue(context.Background(), c, sampleSD())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	info, err := svc.Info(context.Background(), jti)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	for _, m := range info.Methods {
		if m == BindMethodSMSOTP {
			t.Fatalf("sms_otp must be hidden when claims.phone is empty/unverified, got methods=%v", info.Methods)
		}
	}
}

func TestBindService_Info_NotFound(t *testing.T) {
	svc, _, _, _ := newTestBindService(t)
	_, err := svc.Info(context.Background(), "j-nope")
	if !errors.Is(err, ErrBindNotFound) {
		t.Fatalf("expected ErrBindNotFound, got %v", err)
	}
}

// TestBindService_VerifyPassword_Success uid 通过 locator 解析 -> auth 返
// matched -> Store status 推进到 verified,VerifiedMethod=password。
func TestBindService_VerifyPassword_Success(t *testing.T) {
	svc, store, auth, loc := newTestBindService(t)
	auth.verifyPasswordResp.matched = true
	loc.byUsername["alice"] = "u-alice"

	jti, err := svc.Issue(context.Background(), sampleClaims(), sampleSD())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := svc.VerifyPassword(context.Background(), jti, "alice", "Pwd@12345"); err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if auth.calls.pwdUID != "u-alice" || auth.calls.pwdPassword != "Pwd@12345" {
		t.Fatalf("auth call args wrong: %+v", auth.calls)
	}
	sess, err := store.Get(context.Background(), jti)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if sess.Status != BindStatusVerified {
		t.Fatalf("status=%v want verified", sess.Status)
	}
	if sess.VerifiedMethod != BindMethodPassword {
		t.Fatalf("VerifiedMethod=%v", sess.VerifiedMethod)
	}
	if sess.CandidateUID != "u-alice" {
		t.Fatalf("CandidateUID=%q want u-alice", sess.CandidateUID)
	}
}

func TestBindService_VerifyPassword_UnknownUsername(t *testing.T) {
	svc, _, _, _ := newTestBindService(t)
	jti, _ := svc.Issue(context.Background(), sampleClaims(), sampleSD())
	err := svc.VerifyPassword(context.Background(), jti, "ghost", "x")
	if err == nil {
		t.Fatal("unknown username must be rejected")
	}
}

func TestBindService_VerifyPassword_WrongPasswordIncrementsCounter(t *testing.T) {
	svc, store, auth, loc := newTestBindService(t, func(c *BindConfig) {
		c.VerifyMax = 2
	})
	auth.verifyPasswordResp.matched = false
	auth.verifyPasswordResp.reason = "password_mismatch"
	loc.byUsername["alice"] = "u-alice"

	jti, _ := svc.Issue(context.Background(), sampleClaims(), sampleSD())
	if err := svc.VerifyPassword(context.Background(), jti, "alice", "x"); err == nil {
		t.Fatal("wrong password must surface error to caller")
	}
	// 第 2 次还可以(刚好到阈值)
	if err := svc.VerifyPassword(context.Background(), jti, "alice", "x"); err == nil {
		t.Fatal("wrong password must surface error to caller")
	}
	// 第 3 次:超 VerifyMax=2,Limited
	err := svc.VerifyPassword(context.Background(), jti, "alice", "x")
	if !errors.Is(err, ErrBindRateLimited) {
		t.Fatalf("expected ErrBindRateLimited after exceeding VerifyMax, got %v", err)
	}
	// Status 仍是 issued(未通过)
	sess, _ := store.Get(context.Background(), jti)
	if sess.Status != BindStatusIssued {
		t.Fatalf("status=%v want still issued after rejections", sess.Status)
	}
}

// TestBindService_SendSMS_UsesClaimsPhone FR-3.3 核心断言:zone/phone 从
// claims snapshot 取,不接受任何调用方参数(签名上就没有 phone 入参)。
func TestBindService_SendSMS_UsesClaimsPhone(t *testing.T) {
	svc, _, auth, _ := newTestBindService(t)
	jti, _ := svc.Issue(context.Background(), sampleClaims(), sampleSD())
	if err := svc.SendSMS(context.Background(), jti); err != nil {
		t.Fatalf("SendSMS: %v", err)
	}
	if auth.calls.smsZone != "0086" || auth.calls.smsPhone != "13900000000" {
		t.Fatalf("zone/phone not extracted from claims: zone=%q phone=%q",
			auth.calls.smsZone, auth.calls.smsPhone)
	}
}

func TestBindService_SendSMS_PhoneUnavailable(t *testing.T) {
	svc, _, _, _ := newTestBindService(t)
	c := sampleClaims()
	c.PhoneNumber = ""
	c.PhoneVerified = false
	jti, _ := svc.Issue(context.Background(), c, sampleSD())
	if err := svc.SendSMS(context.Background(), jti); err == nil {
		t.Fatal("SendSMS must fail when claims has no verified phone (FR-3.3)")
	}
}

func TestBindService_SendSMS_RateLimited(t *testing.T) {
	svc, _, _, _ := newTestBindService(t, func(c *BindConfig) { c.OTPSendMax = 1 })
	jti, _ := svc.Issue(context.Background(), sampleClaims(), sampleSD())
	if err := svc.SendSMS(context.Background(), jti); err != nil {
		t.Fatalf("first SendSMS: %v", err)
	}
	err := svc.SendSMS(context.Background(), jti)
	if !errors.Is(err, ErrBindRateLimited) {
		t.Fatalf("expected ErrBindRateLimited after exceeding OTPSendMax, got %v", err)
	}
}

// TestBindService_VerifySMS_Success 走通短信验证 -> status verified +
// VerifiedMethod=sms_otp。zone/phone 仍从 claims snapshot 取(FR-3.3)。
func TestBindService_VerifySMS_Success(t *testing.T) {
	svc, store, auth, _ := newTestBindService(t)
	jti, _ := svc.Issue(context.Background(), sampleClaims(), sampleSD())
	if err := svc.VerifySMS(context.Background(), jti, "1234"); err != nil {
		t.Fatalf("VerifySMS: %v", err)
	}
	if auth.calls.verifyZone != "0086" || auth.calls.verifyPhone != "13900000000" || auth.calls.verifyCode != "1234" {
		t.Fatalf("auth.VerifyOIDCBindSMS args wrong: %+v", auth.calls)
	}
	sess, _ := store.Get(context.Background(), jti)
	if sess.Status != BindStatusVerified || sess.VerifiedMethod != BindMethodSMSOTP {
		t.Fatalf("status/method=%v/%v want verified/sms_otp", sess.Status, sess.VerifiedMethod)
	}
}

func TestBindService_VerifySMS_AuthError(t *testing.T) {
	svc, store, auth, _ := newTestBindService(t)
	auth.verifySMSErr = errors.New("code expired")
	jti, _ := svc.Issue(context.Background(), sampleClaims(), sampleSD())
	if err := svc.VerifySMS(context.Background(), jti, "9999"); err == nil {
		t.Fatal("VerifySMS must propagate auth error")
	}
	sess, _ := store.Get(context.Background(), jti)
	if sess.Status != BindStatusIssued {
		t.Fatalf("status must remain issued on auth error, got %v", sess.Status)
	}
}

// TestBindService_VerifySMS_RateLimited 用同一 verify counter 与密码路径
// 共用 — SR-2.1 说 "验证尝试 ≤ 5 次",不区分密码 / 短信。
func TestBindService_VerifySMS_RateLimited(t *testing.T) {
	svc, _, auth, _ := newTestBindService(t, func(c *BindConfig) { c.VerifyMax = 1 })
	auth.verifySMSErr = errors.New("wrong code")

	jti, _ := svc.Issue(context.Background(), sampleClaims(), sampleSD())
	if err := svc.VerifySMS(context.Background(), jti, "1"); err == nil {
		t.Fatal("first failed verify must surface auth error")
	}
	err := svc.VerifySMS(context.Background(), jti, "2")
	if !errors.Is(err, ErrBindRateLimited) {
		t.Fatalf("expected ErrBindRateLimited after exceeding VerifyMax, got %v", err)
	}
}

// TestBindService_ShouldHandle 一致地决定 callback 失败分支接管。
// 仅在 (Enabled && issuer in allowlist && err 是可绑定错误) 时返 true。
func TestBindService_ShouldHandle(t *testing.T) {
	cases := []struct {
		name         string
		enabled      bool
		allowlist    []string
		issuer       string
		err          error
		wantHandle   bool
		whyNotHandle string
	}{
		{"disabled", false, nil, "https://aegis", ErrUnknownUser, false, "flag off"},
		{"empty allowlist denies all", true, nil, "https://aegis", ErrUnknownUser, false, "allowlist empty => deny all"},
		{"issuer not in allowlist", true, []string{"https://google"}, "https://aegis", ErrUnknownUser, false, "wrong issuer"},
		{"in allowlist unknown user", true, []string{"https://aegis"}, "https://aegis", ErrUnknownUser, true, ""},
		{"in allowlist conflict", true, []string{"https://aegis"}, "https://aegis", ErrConflictNeedManual, true, ""},
		{"in allowlist random err", true, []string{"https://aegis"}, "https://aegis", errors.New("db down"), false, "non-bindable err"},
		{"nil err", true, []string{"https://aegis"}, "https://aegis", nil, false, "success path, no bind needed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, _, _ := newTestBindService(t, func(c *BindConfig) {
				c.Enabled = tc.enabled
				c.IssuerAllowlist = tc.allowlist
			})
			got := svc.ShouldHandle(tc.err, &IDTokenClaims{Issuer: tc.issuer})
			if got != tc.wantHandle {
				t.Fatalf("ShouldHandle=%v want=%v (%s)", got, tc.wantHandle, tc.whyNotHandle)
			}
		})
	}
}
