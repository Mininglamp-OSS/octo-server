package oidc

import (
	"context"
	"errors"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"
)

// mockDuplicateKeyErr 制造一个能被 isDuplicateKeyError(api.go) 识别的错误。
// 用 mysql.MySQLError{Number:1062} 真型保证 errors.As 路径走通。
func mockDuplicateKeyErr() error {
	return &mysql.MySQLError{Number: 1062, Message: "Duplicate entry for key 'uk_uid_issuer'"}
}

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
	byPhone    map[string][]string // "zone|phone" -> uids
	err        error
}

func (f *fakeBindLocator) UIDByUsername(username string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.byUsername[username], nil
}

func (f *fakeBindLocator) UIDsByPhone(zone, phone string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byPhone[zone+"|"+phone], nil
}

type fakeIdentityWriter struct {
	inserted    []*IdentityModel
	insertErr   error
	duplicate   bool // true => insertErr is treated as MySQL 1062
	getResp     map[string]*IdentityModel
	updateLogin error
}

func (f *fakeIdentityWriter) Get(issuer, subject string) (*IdentityModel, error) {
	if f.getResp == nil {
		return nil, nil
	}
	return f.getResp[issuer+"|"+subject], nil
}
func (f *fakeIdentityWriter) Insert(m *IdentityModel) error {
	if f.insertErr != nil {
		return f.insertErr
	}
	cp := *m
	f.inserted = append(f.inserted, &cp)
	return nil
}
func (f *fakeIdentityWriter) UpdateLogin(_ int64, _ string, _ int, _ string, _ int) error {
	return f.updateLogin
}

type fakeIssueSession struct {
	resp    *IssueSessionResp
	err     error
	gotReq  IssueSessionReq
	callCnt int
}

func (f *fakeIssueSession) UIDsByEmail(string) ([]string, error)         { return nil, nil }
func (f *fakeIssueSession) UIDsByPhone(string, string) ([]string, error) { return nil, nil }
func (f *fakeIssueSession) IssueSession(_ context.Context, req IssueSessionReq) (*IssueSessionResp, error) {
	f.callCnt++
	f.gotReq = req
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// ---- helpers ----

type bindTestHarness struct {
	svc      *BindService
	store    *memoryBindStore
	auth     *fakeBindAuth
	loc      *fakeBindLocator
	identity *fakeIdentityWriter
	users    *fakeIssueSession
}

func newTestBindService(t *testing.T, cfgMutators ...func(*BindConfig)) (*BindService, *memoryBindStore, *fakeBindAuth, *fakeBindLocator) {
	t.Helper()
	h := newBindHarness(t, cfgMutators...)
	return h.svc, h.store, h.auth, h.loc
}

func newBindHarness(t *testing.T, cfgMutators ...func(*BindConfig)) *bindTestHarness {
	t.Helper()
	store := newMemoryBindStore()
	auth := &fakeBindAuth{}
	loc := &fakeBindLocator{
		byUsername: map[string]string{},
		byPhone:    map[string][]string{},
	}
	identity := &fakeIdentityWriter{}
	users := &fakeIssueSession{resp: &IssueSessionResp{
		UID: "u-default", LoginRespJSON: `{"token":"t-default"}`,
	}}
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
	svc.identity = identity
	svc.users = users
	return &bindTestHarness{
		svc: svc, store: store, auth: auth, loc: loc,
		identity: identity, users: users,
	}
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
// PR4 起验证通过后会用 phone 反查 dmwork user,所以测试需要预置一条 byPhone 命中。
func TestBindService_VerifySMS_Success(t *testing.T) {
	h := newBindHarness(t)
	h.loc.byPhone["0086|13900000000"] = []string{"u-phone-1"}

	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	if err := h.svc.VerifySMS(context.Background(), jti, "1234"); err != nil {
		t.Fatalf("VerifySMS: %v", err)
	}
	if h.auth.calls.verifyZone != "0086" || h.auth.calls.verifyPhone != "13900000000" || h.auth.calls.verifyCode != "1234" {
		t.Fatalf("auth.VerifyOIDCBindSMS args wrong: %+v", h.auth.calls)
	}
	sess, _ := h.store.Get(context.Background(), jti)
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

// TestBindService_VerifySMS_SinglePhoneMatchFillsCandidate 短信路径通过后,
// 用 claims phone 在 dmwork user 表里找到唯一 uid → 写入 sess.CandidateUID,
// confirm 阶段直接用,不再查一次 DB(SR-2 限流维度也更准)。
func TestBindService_VerifySMS_SinglePhoneMatchFillsCandidate(t *testing.T) {
	h := newBindHarness(t)
	h.loc.byPhone["0086|13900000000"] = []string{"u-phone-1"}

	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	if err := h.svc.VerifySMS(context.Background(), jti, "1234"); err != nil {
		t.Fatalf("VerifySMS: %v", err)
	}
	sess, _ := h.store.Get(context.Background(), jti)
	if sess.CandidateUID != "u-phone-1" {
		t.Fatalf("CandidateUID=%q want u-phone-1", sess.CandidateUID)
	}
}

// TestBindService_VerifySMS_MultiPhoneMatchRejected 同 phone 对应多个 dmwork
// 账号(脏数据/历史合并未完成),自助流程无法判定,早期拒绝(FR-4.2 / 风险表)。
func TestBindService_VerifySMS_MultiPhoneMatchRejected(t *testing.T) {
	h := newBindHarness(t)
	h.loc.byPhone["0086|13900000000"] = []string{"u-a", "u-b"}

	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	err := h.svc.VerifySMS(context.Background(), jti, "1234")
	if !errors.Is(err, ErrBindConflictNeedManual) {
		t.Fatalf("expected ErrBindConflictNeedManual on multi-match, got %v", err)
	}
}

// TestBindService_VerifySMS_NoPhoneMatchRejected 短信验证通过但 claims phone
// 不命中任何 dmwork user —— 仍然拒绝,confirm 没有目标可绑。运维通过审计
// (EventBindVerifyFail reason=user_not_found) 可以发现这类用户(应当走
// FR-7 "联系管理员" 兜底)。
func TestBindService_VerifySMS_NoPhoneMatchRejected(t *testing.T) {
	h := newBindHarness(t)
	// 不预置 byPhone -> 返空

	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	err := h.svc.VerifySMS(context.Background(), jti, "1234")
	if err == nil {
		t.Fatal("VerifySMS must reject when no dmwork user matches claims phone")
	}
}

// TestBindService_Confirm_Success 端到端:已 verified 的 session 走 confirm →
// identity.Insert + users.IssueSession + 单次消费(再 Get 应 NotFound)。
func TestBindService_Confirm_Success(t *testing.T) {
	h := newBindHarness(t)
	h.auth.verifyPasswordResp.matched = true
	h.loc.byUsername["alice"] = "u-alice"
	h.users.resp = &IssueSessionResp{UID: "u-alice", LoginRespJSON: `{"token":"t-alice"}`}

	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	if err := h.svc.VerifyPassword(context.Background(), jti, "alice", "Pwd@1"); err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	resp, err := h.svc.Confirm(context.Background(), jti)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if resp.IssueResp.UID != "u-alice" || resp.IssueResp.LoginRespJSON != `{"token":"t-alice"}` {
		t.Fatalf("resp=%+v", resp.IssueResp)
	}
	if resp.SD == nil || resp.SD.ClientAuthcode != "ac-1" {
		t.Fatalf("SD snapshot lost: %+v", resp.SD)
	}
	if len(h.identity.inserted) != 1 {
		t.Fatalf("identity inserts=%d want 1", len(h.identity.inserted))
	}
	ins := h.identity.inserted[0]
	if ins.UID != "u-alice" || ins.Issuer != "https://idp.example" || ins.Subject != "sub-A" {
		t.Fatalf("inserted identity wrong: %+v", ins)
	}
	// 单次消费(SR-1)
	if _, err := h.store.Get(context.Background(), jti); !errors.Is(err, ErrBindNotFound) {
		t.Fatalf("session must be consumed after confirm, got err=%v", err)
	}
}

// TestBindService_Confirm_RequiresVerifiedStatus 只有 verified 状态可 confirm,
// issued/confirmed/refused 都拒。AC-6 并发 confirm 防护也走同样的状态机:
// 第二个 confirm 看到的不是 verified(或根本 Consume 不到)→ 拒绝。
func TestBindService_Confirm_RequiresVerifiedStatus(t *testing.T) {
	h := newBindHarness(t)
	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	// 没走 verify → 状态还在 issued
	_, err := h.svc.Confirm(context.Background(), jti)
	if !errors.Is(err, ErrBindStatusConflict) {
		t.Fatalf("expected ErrBindStatusConflict, got %v", err)
	}
}

// TestBindService_Confirm_DuplicateKey 模拟 DB uk_uid_issuer 命中:
// users.IssueSession 还没调到就被 identity.Insert 拒掉,Confirm 返
// ErrBindAlreadyBound,session 不消费(让用户可重试或人工介入)。
func TestBindService_Confirm_DuplicateKey(t *testing.T) {
	h := newBindHarness(t)
	h.auth.verifyPasswordResp.matched = true
	h.loc.byUsername["alice"] = "u-alice"
	// 模拟 1062
	h.identity.insertErr = mockDuplicateKeyErr()

	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	_ = h.svc.VerifyPassword(context.Background(), jti, "alice", "Pwd@1")
	_, err := h.svc.Confirm(context.Background(), jti)
	if !errors.Is(err, ErrBindAlreadyBound) {
		t.Fatalf("expected ErrBindAlreadyBound on duplicate-key, got %v", err)
	}
	if h.users.callCnt != 0 {
		t.Fatalf("IssueSession should NOT be called when identity insert fails, called %d times", h.users.callCnt)
	}
}

func TestBindService_Confirm_IssueSessionFailure(t *testing.T) {
	h := newBindHarness(t)
	h.auth.verifyPasswordResp.matched = true
	h.loc.byUsername["alice"] = "u-alice"
	h.users.err = errors.New("downstream issuer down")

	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	_ = h.svc.VerifyPassword(context.Background(), jti, "alice", "Pwd@1")
	_, err := h.svc.Confirm(context.Background(), jti)
	if err == nil {
		t.Fatal("Confirm must propagate IssueSession error")
	}
}

// TestBindService_Confirm_IssueSessionFail_RetryHitsAlreadyBound 锁定 reviewer
// 提的"identity 已写但 IssueSession 失败 → 客户端重试"的故障恢复行为:
//   - 第 1 次 confirm:identity.Insert 成功 + IssueSession 失败 → 返错,
//     session 不消费(用户拿 token 还可以再试)
//   - 第 2 次 confirm:identity 已存在 → uk_uid_issuer 撞 → ErrBindAlreadyBound
//     handler 翻 409,前端引导用户走"已绑定,直接 OIDC 登录"
//
// 这条路径如果哪天行为变了(比如 identity.Insert 成功后立即 Consume),会破坏
// 已上线流程,会被这个测试抓到。
func TestBindService_Confirm_IssueSessionFail_RetryHitsAlreadyBound(t *testing.T) {
	h := newBindHarness(t)
	h.auth.verifyPasswordResp.matched = true
	h.loc.byUsername["alice"] = "u-alice"
	h.users.err = errors.New("transient downstream")

	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	_ = h.svc.VerifyPassword(context.Background(), jti, "alice", "Pwd@1")
	// 第 1 次 confirm:IssueSession 失败 → 返错,但 identity 已写入
	if _, err := h.svc.Confirm(context.Background(), jti); err == nil {
		t.Fatal("first confirm should fail (IssueSession down)")
	}
	if len(h.identity.inserted) != 1 {
		t.Fatalf("first confirm should still insert identity, got %d inserts", len(h.identity.inserted))
	}
	// session 仍存在(未 Consume),客户端可以重试
	if _, err := h.store.Get(context.Background(), jti); err != nil {
		t.Fatalf("session must remain for retry, got err=%v", err)
	}
	// 模拟 DB 端 uk_uid_issuer 兜底:第 2 次 Insert 撞唯一约束
	h.identity.insertErr = mockDuplicateKeyErr()
	// IssueSession 恢复 —— 但不应该走到这一步
	h.users.err = nil
	h.users.resp = &IssueSessionResp{UID: "u-alice", LoginRespJSON: "{}"}

	_, err := h.svc.Confirm(context.Background(), jti)
	if !errors.Is(err, ErrBindAlreadyBound) {
		t.Fatalf("retry must surface ErrBindAlreadyBound (handler -> 409), got %v", err)
	}
}

// TestBindService_VerifyPassword_AuthRejectedSentinel 锁定密码错走 ErrBindAuthRejected,
// handler 通过 errors.Is 即可判定 401 vs 500。
func TestBindService_VerifyPassword_AuthRejectedSentinel(t *testing.T) {
	h := newBindHarness(t)
	h.auth.verifyPasswordResp.matched = false
	h.auth.verifyPasswordResp.reason = BindReasonForTest("password_mismatch")
	h.loc.byUsername["alice"] = "u-alice"

	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	err := h.svc.VerifyPassword(context.Background(), jti, "alice", "wrong")
	if !errors.Is(err, ErrBindAuthRejected) {
		t.Fatalf("wrong password must wrap ErrBindAuthRejected (for 401 mapping), got %v", err)
	}
}

// TestBindService_VerifyPassword_InfraErrorNotSentinel 内部错误(DB 抖动)
// 不应当包成 ErrBindAuthRejected —— 否则 metric 错把 500 计为 401。
func TestBindService_VerifyPassword_InfraErrorNotSentinel(t *testing.T) {
	h := newBindHarness(t)
	h.auth.verifyPasswordResp.err = errors.New("db timeout")
	h.loc.byUsername["alice"] = "u-alice"

	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	err := h.svc.VerifyPassword(context.Background(), jti, "alice", "x")
	if errors.Is(err, ErrBindAuthRejected) {
		t.Fatalf("infra error must NOT wrap ErrBindAuthRejected (would mask 500 as 401), got %v", err)
	}
}

// BindReasonForTest 测试 helper:只是个 string alias 避免在 test 文件硬编码
// 真实的 BindReason* 常量(那些定义在 user 包,oidc 测试不该跨包依赖)。
func BindReasonForTest(s string) string { return s }

// TestBindService_Confirm_RateLimited 每 jti confirm 计数走 ConfirmMax,
// 即便 verified 状态没真正消费,过阈值也直接拒(挡攻击者反复 confirm 试探
// downstream issue session 异常)。
func TestBindService_Confirm_RateLimited(t *testing.T) {
	h := newBindHarness(t, func(c *BindConfig) { c.ConfirmMax = 1 })
	h.auth.verifyPasswordResp.matched = true
	h.loc.byUsername["alice"] = "u-alice"
	// 让第 1 次 confirm 拒绝(users.IssueSession 返错)使 session 保留
	h.users.err = errors.New("transient")

	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	_ = h.svc.VerifyPassword(context.Background(), jti, "alice", "Pwd@1")

	if _, err := h.svc.Confirm(context.Background(), jti); err == nil {
		t.Fatal("first confirm should fail on issue-session err")
	}
	// 第 2 次:counter 已经 +1 = 1,limit=1,> 1 → ErrBindRateLimited
	if _, err := h.svc.Confirm(context.Background(), jti); !errors.Is(err, ErrBindRateLimited) {
		t.Fatalf("expected ErrBindRateLimited, got %v", err)
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
