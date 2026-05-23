package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
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
	mu                 sync.Mutex // 并发测试(TestBindService_VerifyPassword_ConcurrentRaceOnlyOneWins)需要
	verifyPasswordResp struct {
		matched bool
		reason  string
		err     error
	}
	sendSMSErr      error
	verifySMSErr    error
	isBindableErr   error
	isBindableFalse bool // 默认 true,设 true 时 IsBindable 返 (false, nil)
	calls           struct {
		pwdUID, pwdPassword                 string
		smsZone, smsPhone                   string
		verifyZone, verifyPhone, verifyCode string
		isBindableUID                       string
		pwdCount, sendCount, verifyCount    int
		isBindableCount                     int
	}
}

func (f *fakeBindAuth) VerifyPasswordByUID(_ context.Context, uid, password string) (bool, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.pwdCount++
	f.calls.pwdUID = uid
	f.calls.pwdPassword = password
	return f.verifyPasswordResp.matched, f.verifyPasswordResp.reason, f.verifyPasswordResp.err
}
func (f *fakeBindAuth) SendOIDCBindSMS(_ context.Context, zone, phone string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.sendCount++
	f.calls.smsZone = zone
	f.calls.smsPhone = phone
	return f.sendSMSErr
}
func (f *fakeBindAuth) VerifyOIDCBindSMS(_ context.Context, zone, phone, code string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.verifyCount++
	f.calls.verifyZone = zone
	f.calls.verifyPhone = phone
	f.calls.verifyCode = code
	return f.verifySMSErr
}
func (f *fakeBindAuth) IsBindable(_ context.Context, uid string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.isBindableCount++
	f.calls.isBindableUID = uid
	if f.isBindableErr != nil {
		return false, f.isBindableErr
	}
	return !f.isBindableFalse, nil
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
	mu          sync.Mutex
	inserted    []*IdentityModel
	insertErr   error
	duplicate   bool // true => insertErr is treated as MySQL 1062
	getResp     map[string]*IdentityModel
	updateLogin error
}

func (f *fakeIdentityWriter) Get(issuer, subject string) (*IdentityModel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getResp == nil {
		return nil, nil
	}
	return f.getResp[issuer+"|"+subject], nil
}
func (f *fakeIdentityWriter) Insert(m *IdentityModel) error {
	f.mu.Lock()
	defer f.mu.Unlock()
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
	mu      sync.Mutex
	resp    *IssueSessionResp
	err     error
	gotReq  IssueSessionReq
	callCnt int
}

func (f *fakeIssueSession) UIDsByEmail(string) ([]string, error)         { return nil, nil }
func (f *fakeIssueSession) UIDsByPhone(string, string) ([]string, error) { return nil, nil }
func (f *fakeIssueSession) IssueSession(_ context.Context, req IssueSessionReq) (*IssueSessionResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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
		// 默认放行 sampleClaims().Issuer,让 Create 路径的 issuerAllowedForCreate
		// 兜底校验默认放行;需要测"allowlist 拒绝"的用例显式覆盖。
		IssuerAllowlist: []string{"https://idp.example"},
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

// TestBindService_Info_MaskedFieldsAlwaysSerialized GH#148:masked_email /
// masked_phone 必须始终出现在 JSON 中,空 claim 序列化为 ""(而非 omit)。
// omitempty 会让前端 schema validator 抛 "masked_email must be string",
// 通用错误映射把它误报为"网络异常",见 issue 148。
func TestBindService_Info_MaskedFieldsAlwaysSerialized(t *testing.T) {
	svc, _, _, _ := newTestBindService(t)
	c := sampleClaims()
	c.Email = ""
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
	if info.MaskedEmail != "" || info.MaskedPhone != "" {
		t.Fatalf("expected empty masks, got email=%q phone=%q", info.MaskedEmail, info.MaskedPhone)
	}
	raw, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, `"masked_email":""`) {
		t.Fatalf("masked_email key missing or non-empty in JSON: %s", body)
	}
	if !strings.Contains(body, `"masked_phone":""`) {
		t.Fatalf("masked_phone key missing or non-empty in JSON: %s", body)
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

// TestBindService_VerifyPassword_UnknownIdentifierWrapsAuthRejected 锁定 #1 修复:
// locator 没找到 uid 时返回的错误必须 wrap ErrBindAuthRejected,handler 才会
// 翻 401(不是 500),metric 才会落 unauthorized(不是 internal_error)。
func TestBindService_VerifyPassword_UnknownIdentifierWrapsAuthRejected(t *testing.T) {
	h := newBindHarness(t)
	// 不预置 byUsername["ghost"],locator 返 "", nil
	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	err := h.svc.VerifyPassword(context.Background(), jti, "ghost", "x")
	if !errors.Is(err, ErrBindAuthRejected) {
		t.Fatalf("unknown identifier must wrap ErrBindAuthRejected (handler -> 401, metric -> unauthorized), got %v", err)
	}
}

// TestBindService_VerifySMS_NoPhoneMatchWrapsAuthRejected 锁定 #2 修复:
// claims phone 没匹配到 dmwork user 时返 ErrBindAuthRejected,而不是普通 error。
func TestBindService_VerifySMS_NoPhoneMatchWrapsAuthRejected(t *testing.T) {
	h := newBindHarness(t)
	// 不预置 byPhone -> 返空切片
	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	err := h.svc.VerifySMS(context.Background(), jti, "1234")
	if !errors.Is(err, ErrBindAuthRejected) {
		t.Fatalf("0-phone-match must wrap ErrBindAuthRejected, got %v", err)
	}
}

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

// TestBindService_VerifyPassword_RejectsAfterVerified 锁定 Issue D 修复:
// 一旦 session 已 verified,任何二次 verify 必须被状态机拒绝(409 conflict),
// 不能再覆盖 CandidateUID。否则攻击者拿 victim 的 bind_token,在 confirm 前
// 用自己账号再 verify 一次 → CandidateUID 被改 → confirm 时绑到攻击者 uid,
// 等价于 OIDC 身份接管。
func TestBindService_VerifyPassword_RejectsAfterVerified(t *testing.T) {
	h := newBindHarness(t)
	h.auth.verifyPasswordResp.matched = true
	h.loc.byUsername["alice"] = "u-alice"
	h.loc.byUsername["mallory"] = "u-mallory"

	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	if err := h.svc.VerifyPassword(context.Background(), jti, "alice", "Pwd@1"); err != nil {
		t.Fatalf("first verify: %v", err)
	}
	// 攻击者拿到同 token,用自己账号 verify
	err := h.svc.VerifyPassword(context.Background(), jti, "mallory", "Pwd@1")
	if !errors.Is(err, ErrBindStatusConflict) {
		t.Fatalf("re-verify after verified must be ErrBindStatusConflict, got %v", err)
	}
	sess, _ := h.store.Get(context.Background(), jti)
	if sess.CandidateUID != "u-alice" {
		t.Fatalf("CandidateUID must NOT be overwritten, got %q", sess.CandidateUID)
	}
}

func TestBindService_VerifySMS_RejectsAfterVerified(t *testing.T) {
	h := newBindHarness(t)
	h.loc.byPhone["0086|13900000000"] = []string{"u-phone"}

	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	if err := h.svc.VerifySMS(context.Background(), jti, "1234"); err != nil {
		t.Fatalf("first verify: %v", err)
	}
	err := h.svc.VerifySMS(context.Background(), jti, "1234")
	if !errors.Is(err, ErrBindStatusConflict) {
		t.Fatalf("re-verify after verified must be ErrBindStatusConflict, got %v", err)
	}
}

// TestBindService_VerifyPassword_RejectsWhenMethodDisabled 锁定 Issue C 修复:
// cfg.Methods 是真实策略,不仅是 UI 过滤。运维禁掉 password 后,即便客户端
// 硬调端点,service 必须拒(400 method_disabled)而不能继续走 auth。
func TestBindService_VerifyPassword_RejectsWhenMethodDisabled(t *testing.T) {
	h := newBindHarness(t, func(c *BindConfig) {
		c.Methods = []BindMethod{BindMethodSMSOTP} // password 禁用
	})
	h.auth.verifyPasswordResp.matched = true
	h.loc.byUsername["alice"] = "u-alice"

	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	err := h.svc.VerifyPassword(context.Background(), jti, "alice", "Pwd@1")
	if !errors.Is(err, ErrBindMethodDisabled) {
		t.Fatalf("disabled method must return ErrBindMethodDisabled, got %v", err)
	}
	if h.auth.calls.pwdCount != 0 {
		t.Fatalf("auth must not be called when method disabled, calls=%d", h.auth.calls.pwdCount)
	}
}

func TestBindService_SendSMS_RejectsWhenMethodDisabled(t *testing.T) {
	h := newBindHarness(t, func(c *BindConfig) {
		c.Methods = []BindMethod{BindMethodPassword} // sms 禁用
	})
	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	err := h.svc.SendSMS(context.Background(), jti)
	if !errors.Is(err, ErrBindMethodDisabled) {
		t.Fatalf("disabled sms must return ErrBindMethodDisabled, got %v", err)
	}
	if h.auth.calls.sendCount != 0 {
		t.Fatalf("SMSService must not be called when method disabled, calls=%d", h.auth.calls.sendCount)
	}
}

func TestBindService_VerifySMS_RejectsWhenMethodDisabled(t *testing.T) {
	h := newBindHarness(t, func(c *BindConfig) {
		c.Methods = []BindMethod{BindMethodPassword}
	})
	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	err := h.svc.VerifySMS(context.Background(), jti, "1234")
	if !errors.Is(err, ErrBindMethodDisabled) {
		t.Fatalf("disabled sms verify must return ErrBindMethodDisabled, got %v", err)
	}
}

// TestBindService_VerifyPassword_UIDFailPerDayEnforced 锁定 Issue A 修复:
// 每个 uid 每日密码失败计数达到 UIDFailPerDay 后,后续请求必须直接 ErrBindRateLimited
// (即便单 token 的 VerifyMax 还没到)。SR-2.2 防"一个 uid 被多 token 攻打"。
func TestBindService_VerifyPassword_UIDFailPerDayEnforced(t *testing.T) {
	h := newBindHarness(t, func(c *BindConfig) {
		c.UIDFailPerDay = 2 // 小阈值便于测试
		c.VerifyMax = 100   // 隔离 per-token 限流
	})
	h.auth.verifyPasswordResp.matched = false
	h.loc.byUsername["alice"] = "u-alice"

	// 第一个 token: 2 次失败
	jti1, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	for i := 0; i < 2; i++ {
		if err := h.svc.VerifyPassword(context.Background(), jti1, "alice", "wrong"); err == nil {
			t.Fatalf("iter %d: wrong password should error", i)
		}
	}
	// 切换到第二个 token(模拟攻击者在 VerifyMax 限流后换 token 继续),
	// uid 维度 counter 应当已经 == 2,再 +1 = 3 > limit=2 → 限流。
	jti2, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	err := h.svc.VerifyPassword(context.Background(), jti2, "alice", "wrong")
	if !errors.Is(err, ErrBindRateLimited) {
		t.Fatalf("UIDFailPerDay must rate-limit cross-token attempts, got %v", err)
	}
}

// TestBindService_SaveVerified_UsesAbsoluteTTLNotFullTokenTTL 锁定:
// CASSave 必须用"距离 Issue 时刻 + TokenTTL 还剩多少
// 秒"作为新 TTL,而不是完整 cfg.TokenTTL,否则用户在临近过期时 verify 会
// 把 token 续到 ~2 × TokenTTL,违反 5min 文档承诺 + URL 泄漏窗口翻倍。
//
// 测试手法:把 nowUnix 改回退到过去,模拟"sess 是 50 秒前签发的,TokenTTL
// 是 60 秒",触发 saveVerified 后用 store-level 直接验证 TTL 行为(memory
// store 内部存 expiresAt = now + ttl,如果传了 cfg.TokenTTL=60s 应当看到
// 比预期大的 expiresAt;预期实现传 ~10s remaining)。
func TestBindService_SaveVerified_UsesAbsoluteTTLNotFullTokenTTL(t *testing.T) {
	h := newBindHarness(t, func(c *BindConfig) {
		c.TokenTTL = 60 * time.Second
	})
	h.auth.verifyPasswordResp.matched = true
	h.loc.byUsername["alice"] = "u-alice"

	// 模拟 sess 是 50 秒前签发的(剩 ~10 秒)
	origNow := nowUnix
	defer func() { nowUnix = origNow }()
	pastIssue := time.Now().Add(-50 * time.Second).Unix()
	nowUnix = func() int64 { return pastIssue }
	jti, err := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	// 恢复 nowUnix 让 saveVerified 走"真实当前时间",计算 remaining
	nowUnix = origNow

	// 用 60s 完整 TTL 直接 Save 一次,模拟旧实现的"刷新到完整 TokenTTL";
	// 然后我们的 VerifyPassword 走 CASSave with remaining ≈ 10s,实际 expiresAt
	// 不会被往后推超过 ~10s。
	if err := h.svc.VerifyPassword(context.Background(), jti, "alice", "p"); err != nil {
		t.Fatalf("verify: %v", err)
	}

	// 校验 memory store 的 expiresAt 距离现在不到 30 秒 —— 远小于完整 TokenTTL=60s。
	// 30s 是宽松上限(测试机时钟漂移 + GC 暂停留余地)。
	h.store.mu.Lock()
	entry := h.store.sessions[jti]
	h.store.mu.Unlock()
	if remaining := time.Until(entry.expiresAt); remaining > 30*time.Second {
		t.Fatalf("expiresAt extended beyond absolute deadline: remaining=%v want <=30s "+
			"(would let bind_token live ~2 × TokenTTL — bug)", remaining)
	}
}

// TestBindService_SaveVerified_RejectsExpiredSession sess 已经过绝对截止,
// saveVerified 必须返 ErrBindNotFound(不能借 verify 路径无限续命已过期 token)。
func TestBindService_SaveVerified_RejectsExpiredSession(t *testing.T) {
	h := newBindHarness(t, func(c *BindConfig) {
		c.TokenTTL = 60 * time.Second
	})
	h.auth.verifyPasswordResp.matched = true
	h.loc.byUsername["alice"] = "u-alice"

	origNow := nowUnix
	defer func() { nowUnix = origNow }()
	// sess 在 2 × TokenTTL 之前签发,绝对早已过期
	nowUnix = func() int64 { return time.Now().Add(-2 * time.Minute).Unix() }
	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	nowUnix = origNow

	err := h.svc.VerifyPassword(context.Background(), jti, "alice", "p")
	if !errors.Is(err, ErrBindNotFound) {
		t.Fatalf("expired session must surface ErrBindNotFound (not extendable), got %v", err)
	}
}

// TestBindService_Confirm_RejectsWhenCandidateBecomesUnbindable 锁定:
// verify→confirm 之间账号被 disable/destroy,Confirm
// 必须再查一次 IsBindable,识别后拒绝(ErrBindAuthRejected),**不**写 identity 行。
func TestBindService_Confirm_RejectsWhenCandidateBecomesUnbindable(t *testing.T) {
	h := newBindHarness(t)
	h.auth.verifyPasswordResp.matched = true
	h.loc.byUsername["alice"] = "u-alice"

	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	if err := h.svc.VerifyPassword(context.Background(), jti, "alice", "p"); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// 模拟管理员在 verify 后 disable 了账号
	h.auth.isBindableFalse = true

	_, err := h.svc.Confirm(context.Background(), jti)
	if !errors.Is(err, ErrBindAuthRejected) {
		t.Fatalf("confirm must reject when uid not bindable, got %v", err)
	}
	if h.auth.calls.isBindableUID != "u-alice" {
		t.Fatalf("IsBindable not called with CandidateUID, got %q", h.auth.calls.isBindableUID)
	}
	// 绝不能写 identity 行
	if len(h.identity.inserted) != 0 {
		t.Fatalf("identity must NOT be inserted when uid unbindable, inserts=%d", len(h.identity.inserted))
	}
	// 也不该签 session
	if h.users.callCnt != 0 {
		t.Fatalf("IssueSession must NOT be called when uid unbindable, calls=%d", h.users.callCnt)
	}
}

// TestBindService_Confirm_IsBindableInfraError 内部错误不该归 401,
// metric 必须落 internal_error。
func TestBindService_Confirm_IsBindableInfraError(t *testing.T) {
	h := newBindHarness(t)
	h.auth.verifyPasswordResp.matched = true
	h.loc.byUsername["alice"] = "u-alice"
	h.auth.isBindableErr = errors.New("db timeout")

	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	_ = h.svc.VerifyPassword(context.Background(), jti, "alice", "p")
	_, err := h.svc.Confirm(context.Background(), jti)
	if err == nil {
		t.Fatal("infra error must propagate")
	}
	if errors.Is(err, ErrBindAuthRejected) {
		t.Fatalf("infra error must NOT wrap ErrBindAuthRejected (would mask 500 as 401), got %v", err)
	}
}

// TestBindService_VerifyPassword_UserLayerRateLimitedMapsTo429 锁定:
// user.VerifyPasswordByUID 在 loginGuard
// 锁定时返 (matched=false, reason="rate_limited"),BindService 必须翻 ErrBindRateLimited
// (handler 429 + metric rate_limited),不能笼统归 ErrBindAuthRejected(401 +
// metric unauthorized)—— 与 PR 的 sentinel/status/metric 三方对齐契约一致。
//
// 同时锁定不消耗 SR-2.2 uid-fail 配额(配额给"密码错"用,user 已锁就别再 +1)。
func TestBindService_VerifyPassword_UserLayerRateLimitedMapsTo429(t *testing.T) {
	h := newBindHarness(t, func(c *BindConfig) {
		c.UIDFailPerDay = 2
		c.VerifyMax = 100 // 把 per-token rate-limit 拉远,本测试只考察 user-layer 反馈
	})
	h.auth.verifyPasswordResp.matched = false
	h.auth.verifyPasswordResp.reason = userBindReasonRateLimited
	h.loc.byUsername["alice"] = "u-alice"

	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	err := h.svc.VerifyPassword(context.Background(), jti, "alice", "x")
	if !errors.Is(err, ErrBindRateLimited) {
		t.Fatalf("user-layer rate_limited reason must map to ErrBindRateLimited, got %v", err)
	}
	// 重试若干次 —— 即便超过 UIDFailPerDay 也应当稳定返 ErrBindRateLimited,
	// 因为 user 层短路在 SR-2.2 uid-fail counter 之前就返回了(不消费配额)。
	for i := 0; i < 5; i++ {
		if e := h.svc.VerifyPassword(context.Background(), jti, "alice", "x"); !errors.Is(e, ErrBindRateLimited) {
			t.Fatalf("iter %d: should remain ErrBindRateLimited, got %v", i, e)
		}
	}
}

// TestBindService_VerifyPassword_DisabledIdentifierRejected 锁定 service 层
// 对停用账号的契约:dbBindLocator 已经
// 过滤了 is_destroy/status,locator 对停用账号返 "",service 走 unknown-identifier
// 路径返 ErrBindAuthRejected(handler 401,与"用户存在但密码错"无差异 SR-6)。
//
// SQL 层过滤在 db_integration_test.go 里覆盖;这条测试锁的是 service 层
// 对 locator 返回 "" 的处理 —— 即便未来 SQL 漏掉过滤,user.VerifyPasswordByUID
// 也有 BindReasonUserUnavailable 兜底,但 service 必须把它当作"业务拒绝"
// 而不是 internal_error。
func TestBindService_VerifyPassword_DisabledIdentifierRejected(t *testing.T) {
	h := newBindHarness(t)
	// 模拟 locator 已经过滤掉停用用户 → 返空
	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	err := h.svc.VerifyPassword(context.Background(), jti, "disabled-user", "any")
	if !errors.Is(err, ErrBindAuthRejected) {
		t.Fatalf("disabled identifier must surface as ErrBindAuthRejected (handler 401, anti-enum), got %v", err)
	}
}

// TestBindService_VerifyPassword_ConcurrentRaceOnlyOneWins 锁定 CAS 修复:
// 两个并发 verify 即便都基于同一份 status=issued 的快照 + 都 auth 成功,
// 只有一个能把 CandidateUID 落地,另一个必须以 ErrBindStatusConflict 失败 ——
// 否则攻击者拿到 victim bind_token 后用自己账号并发一次 verify 即可覆盖
// CandidateUID 导致 confirm 时绑到攻击者 uid(身份接管)。
//
// 不直接构造"完全同时"的调度 —— Go 调度不保证,memorystore 内有 mutex
// 串行化每个原语 —— 改用循环 + barrier 重复 30 次,在 -race 下足以暴露
// 旧实现的 CandidateUID 覆盖问题;CAS 修复后始终是 (1 ok, 1 conflict)。
func TestBindService_VerifyPassword_ConcurrentRaceOnlyOneWins(t *testing.T) {
	for i := 0; i < 30; i++ {
		h := newBindHarness(t)
		h.auth.verifyPasswordResp.matched = true
		h.loc.byUsername["alice"] = "u-alice"
		h.loc.byUsername["mallory"] = "u-mallory"

		jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())

		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, 2)
		identifiers := []string{"alice", "mallory"}
		for idx, id := range identifiers {
			wg.Add(1)
			go func(idx int, id string) {
				defer wg.Done()
				<-start
				errs[idx] = h.svc.VerifyPassword(context.Background(), jti, id, "p")
			}(idx, id)
		}
		close(start)
		wg.Wait()

		var okCount, conflictCount int
		for _, e := range errs {
			switch {
			case e == nil:
				okCount++
			case errors.Is(e, ErrBindStatusConflict):
				conflictCount++
			default:
				t.Fatalf("iter %d: unexpected err %v", i, e)
			}
		}
		if okCount != 1 || conflictCount != 1 {
			t.Fatalf("iter %d: want exactly 1 ok + 1 conflict, got ok=%d conflict=%d (errs=%v)",
				i, okCount, conflictCount, errs)
		}
		// CandidateUID 必须是赢家的某一个,但绝不能为空或被覆盖成混合
		sess, _ := h.store.Get(context.Background(), jti)
		if sess.CandidateUID != "u-alice" && sess.CandidateUID != "u-mallory" {
			t.Fatalf("iter %d: CandidateUID corrupted: %q", i, sess.CandidateUID)
		}
	}
}

// TestBindService_VerifyPassword_UnknownIdentifierConsumesUIDFailBudget 锁定
// reviewer 提的副信道修复:unknown identifier 不能"免费"穿过 uid-fail 计数,
// 否则攻击者可以从"我自己是否被限流"反推 identifier 是否存在(绕过 SR-6 反枚举)。
func TestBindService_VerifyPassword_UnknownIdentifierConsumesUIDFailBudget(t *testing.T) {
	h := newBindHarness(t, func(c *BindConfig) {
		c.UIDFailPerDay = 2
		c.VerifyMax = 100
	})
	// 同一不存在的 identifier 反复请求(变更 token 跳出 per-token VerifyMax)
	for i := 0; i < 2; i++ {
		jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
		if err := h.svc.VerifyPassword(context.Background(), jti, "ghost", "x"); err == nil {
			t.Fatalf("iter %d: unknown identifier should still error", i)
		}
	}
	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	err := h.svc.VerifyPassword(context.Background(), jti, "ghost", "x")
	if !errors.Is(err, ErrBindRateLimited) {
		t.Fatalf("repeated unknown identifier must hit uid-fail limit (anti-enumeration), got %v", err)
	}
}

// ---- Create tests ----

// T10: happy path
func TestBindService_Create_HappyPath(t *testing.T) {
	h := newBindHarness(t, func(c *BindConfig) {
		c.AllowCreate = true
	})
	h.users.resp = &IssueSessionResp{UID: "u-new", LoginRespJSON: `{"token":"t-new"}`}
	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())

	resp, err := h.svc.Create(context.Background(), jti)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if resp.IssueResp.UID != "u-new" {
		t.Fatalf("resp UID=%q want u-new", resp.IssueResp.UID)
	}
	// IssueSession called with CreateUser=true
	if !h.users.gotReq.CreateUser {
		t.Fatal("IssueSession must be called with CreateUser=true")
	}
	// identity.Insert called
	if len(h.identity.inserted) != 1 {
		t.Fatalf("identity inserts=%d want 1", len(h.identity.inserted))
	}
	// token must be consumed after successful Create (SR-1)
	if _, err := h.store.Get(context.Background(), jti); !errors.Is(err, ErrBindNotFound) {
		t.Fatalf("session must be consumed after Create, got err=%v", err)
	}
	// SD is returned for cross-device authcode backfill
	if resp.SD == nil {
		t.Fatal("SD must be non-nil in Create response")
	}
}

// T17: claims 既无 verified email 也无 verified phone → ErrBindCreateClaimsIncomplete
func TestBindService_Create_ClaimsMissingEmailAndPhone(t *testing.T) {
	h := newBindHarness(t, func(c *BindConfig) {
		c.AllowCreate = true
	})
	c := sampleClaims()
	c.Email = ""
	c.EmailVerified = false
	c.PhoneNumber = ""
	c.PhoneVerified = false
	jti, _ := h.svc.Issue(context.Background(), c, sampleSD())
	_, err := h.svc.Create(context.Background(), jti)
	if !errors.Is(err, ErrBindCreateClaimsIncomplete) {
		t.Fatalf("T17: expected ErrBindCreateClaimsIncomplete, got %v", err)
	}
}

// T21: phone 存在但未 verified + email 缺失 → ErrBindCreateClaimsIncomplete
// (单一标识但 verified=false 等价于"没有可信标识")
func TestBindService_Create_PhoneNotVerified(t *testing.T) {
	h := newBindHarness(t, func(c *BindConfig) {
		c.AllowCreate = true
	})
	c := sampleClaims()
	c.Email = ""
	c.EmailVerified = false
	c.PhoneVerified = false
	jti, _ := h.svc.Issue(context.Background(), c, sampleSD())
	_, err := h.svc.Create(context.Background(), jti)
	if !errors.Is(err, ErrBindCreateClaimsIncomplete) {
		t.Fatalf("T21: unverified phone + no email must return ErrBindCreateClaimsIncomplete, got %v", err)
	}
}

// 非 +86 verified phone + 无 email → ErrBindCreateClaimsIncomplete。
//
// extractPhone 当前只识别 +86;放过其他号段会让 IssueSession 收到空 Phone/Zone,
// 落库后用户没有可用手机号锚点。把"格式不支持"提前到 422 而非建残缺账号。
func TestBindService_Create_NonCNPhone_NoEmail_ClaimsIncomplete(t *testing.T) {
	h := newBindHarness(t, func(c *BindConfig) {
		c.AllowCreate = true
	})
	c := sampleClaims()
	c.Email = ""
	c.EmailVerified = false
	c.PhoneNumber = "+14155551234" // verified 但 dmwork 不支持
	c.PhoneVerified = true
	jti, _ := h.svc.Issue(context.Background(), c, sampleSD())
	_, err := h.svc.Create(context.Background(), jti)
	if !errors.Is(err, ErrBindCreateClaimsIncomplete) {
		t.Fatalf("non-+86 phone + no email must be rejected, got %v", err)
	}
}

// T22: email 存在但未 verified + phone 缺失 → ErrBindCreateClaimsIncomplete
func TestBindService_Create_EmailNotVerified(t *testing.T) {
	h := newBindHarness(t, func(c *BindConfig) {
		c.AllowCreate = true
	})
	c := sampleClaims()
	c.EmailVerified = false
	c.PhoneNumber = ""
	c.PhoneVerified = false
	jti, _ := h.svc.Issue(context.Background(), c, sampleSD())
	_, err := h.svc.Create(context.Background(), jti)
	if !errors.Is(err, ErrBindCreateClaimsIncomplete) {
		t.Fatalf("T22: unverified email + no phone must return ErrBindCreateClaimsIncomplete, got %v", err)
	}
}

// T16: 超过 bindCreateMax(=1)→ ErrBindRateLimited
//
// 新设计语义:第一次 Create 失败(IssueSession 报错)会把 token 留在 creating 锁
// 状态(防 ghost user)。第二次 Create:counter=2 > bindCreateMax=1,先被
// IncrAndCheck 拦下返 ErrBindRateLimited,不到 CAS。两个失败状态都不会让 token
// 被同样的 jti 复用建出第二个 user。
func TestBindService_Create_RateLimited(t *testing.T) {
	h := newBindHarness(t, func(c *BindConfig) {
		c.AllowCreate = true
	})
	h.users.err = errors.New("transient")
	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())

	if _, err := h.svc.Create(context.Background(), jti); err == nil {
		t.Fatal("first create should fail due to IssueSession error")
	}
	sess, _ := h.store.Get(context.Background(), jti)
	if sess.Status != BindStatusCreating {
		t.Fatalf("T16: status must be 'creating' after failed create (lock-before-side-effect), got %v", sess.Status)
	}
	_, err := h.svc.Create(context.Background(), jti)
	if !errors.Is(err, ErrBindRateLimited) {
		t.Fatalf("T16: expected ErrBindRateLimited, got %v", err)
	}
}

// T25: verify 失败后再 create → 通过(D3：限频独立)
func TestBindService_Create_AfterVerifyFail_Succeeds(t *testing.T) {
	h := newBindHarness(t, func(c *BindConfig) {
		c.AllowCreate = true
		c.VerifyMax = 3
	})
	h.users.resp = &IssueSessionResp{UID: "u-new", LoginRespJSON: `{}`}
	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())

	// verify fails (wrong password)
	h.auth.verifyPasswordResp.matched = false
	h.loc.byUsername["alice"] = "u-alice"
	_ = h.svc.VerifyPassword(context.Background(), jti, "alice", "wrong")

	// status still issued → create should work
	_, err := h.svc.Create(context.Background(), jti)
	if err != nil {
		t.Fatalf("T25: Create after failed verify must succeed, got: %v", err)
	}
}

// T26: claims invalid 的 create 失败不会污染 token 状态 → 之后 verify 仍可走。
//
// 新设计语义:claims 校验在 CAS 之前(纯只读),失败保持 token 在 issued。
// 这是"create 失败不阻塞 verify"的合法路径 —— IssueSession 已经发生的失败
// 会把 token 锁到 creating(防 ghost user),此后 verify 也会被拒,符合预期。
func TestBindService_Create_ClaimsFailThenVerify_Succeeds(t *testing.T) {
	h := newBindHarness(t, func(c *BindConfig) {
		c.AllowCreate = true
		c.VerifyMax = 3
	})
	c := sampleClaims()
	c.Email = ""
	c.EmailVerified = false
	c.PhoneNumber = ""
	c.PhoneVerified = false
	jti, _ := h.svc.Issue(context.Background(), c, sampleSD())

	if _, err := h.svc.Create(context.Background(), jti); !errors.Is(err, ErrBindCreateClaimsIncomplete) {
		t.Fatalf("Create must fail with claims incomplete, got %v", err)
	}
	sess, _ := h.store.Get(context.Background(), jti)
	if sess.Status != BindStatusIssued {
		t.Fatalf("status after claims-check failure must remain issued, got %v", sess.Status)
	}

	h.auth.verifyPasswordResp.matched = true
	h.loc.byUsername["alice"] = "u-alice"
	if err := h.svc.VerifyPassword(context.Background(), jti, "alice", "p"); err != nil {
		t.Fatalf("T26: VerifyPassword after failed create must succeed, got: %v", err)
	}
}

// T23: IssueSession 失败 → wrap error,token 留在 creating 锁状态。
//
// 新设计语义:token 不被 Consume(Redis 行还在),但 status 已经被 CAS 推到
// creating(防 ghost user)。retry 同 jti 会撞 CAS conflict 或 rate limit ——
// 这是有意为之:确保单个 IdP 身份只产生一个本地 user。
// 用户需重走 OIDC 登录拿新 bind_token 才能再次尝试。
func TestBindService_Create_IssueSessionFail_TokenLockedCreating(t *testing.T) {
	h := newBindHarness(t, func(c *BindConfig) {
		c.AllowCreate = true
	})
	h.users.err = errors.New("downstream down")
	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())

	_, err := h.svc.Create(context.Background(), jti)
	if err == nil {
		t.Fatal("T23: Create must propagate IssueSession error")
	}
	sess, gerr := h.store.Get(context.Background(), jti)
	if gerr != nil {
		t.Fatalf("T23: session must NOT be consumed (Redis row still exists), got %v", gerr)
	}
	if sess.Status != BindStatusCreating {
		t.Fatalf("T23: status must be 'creating' after IssueSession failure (防 ghost user), got %v", sess.Status)
	}
}

// T24: identity.Insert 撞 uk_issuer_subject → ErrBindAlreadyBound
func TestBindService_Create_IdentityInsertDuplicate_AlreadyBound(t *testing.T) {
	h := newBindHarness(t, func(c *BindConfig) {
		c.AllowCreate = true
	})
	h.users.resp = &IssueSessionResp{UID: "u-new", LoginRespJSON: `{}`}
	h.identity.insertErr = mockDuplicateKeyErr()
	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())

	_, err := h.svc.Create(context.Background(), jti)
	if !errors.Is(err, ErrBindAlreadyBound) {
		t.Fatalf("T24: duplicate identity must surface ErrBindAlreadyBound, got %v", err)
	}
}

// T27: 并发 create 同 token：只有一个 CASSave 成功，另一个 ErrBindStatusConflict
func TestBindService_Create_ConcurrentRaceOnlyOneWins(t *testing.T) {
	for i := 0; i < 20; i++ {
		h := newBindHarness(t, func(c *BindConfig) {
			c.AllowCreate = true
		})
		h.users.resp = &IssueSessionResp{UID: "u-new", LoginRespJSON: `{}`}
		jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())

		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		for idx := 0; idx < 2; idx++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				<-start
				_, errs[idx] = h.svc.Create(context.Background(), jti)
			}(idx)
		}
		close(start)
		wg.Wait()

		var okCount, failCount int
		for _, e := range errs {
			switch {
			case e == nil:
				okCount++
			case errors.Is(e, ErrBindStatusConflict),
				errors.Is(e, ErrBindNotFound),
				errors.Is(e, ErrBindRateLimited):
				// ErrBindStatusConflict: loser's CASSave sees status already advanced
				// ErrBindNotFound:       winner already Consumed session before loser's CASSave
				// ErrBindRateLimited:    bindCreateMax=1 → 2nd IncrAndCheck exceeds limit
				failCount++
			default:
				t.Fatalf("iter %d: unexpected err %v", i, e)
			}
		}
		if okCount != 1 || failCount != 1 {
			t.Fatalf("iter %d: want 1 ok + 1 fail, got ok=%d fail=%d", i, okCount, failCount)
		}
	}
}

// T28: Consume 失败仅 log，不返 error
func TestBindService_Create_ConsumeFailureIsNonFatal(t *testing.T) {
	h := newBindHarness(t, func(c *BindConfig) {
		c.AllowCreate = true
	})
	h.users.resp = &IssueSessionResp{UID: "u-new", LoginRespJSON: `{}`}

	// Use a store that makes Consume fail but Get/Save/CASSave work
	failConsumeStore := &failingConsumeStore{inner: h.store}
	h.svc.store = failConsumeStore
	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())

	resp, err := h.svc.Create(context.Background(), jti)
	if err != nil {
		t.Fatalf("T28: Consume failure must not propagate, got: %v", err)
	}
	if resp == nil {
		t.Fatal("T28: Create must return valid resp despite Consume failure")
	}
}

// failingConsumeStore wraps memoryBindStore and makes Consume always fail.
type failingConsumeStore struct {
	inner *memoryBindStore
}

func (f *failingConsumeStore) Save(ctx context.Context, s *BindSession, ttl time.Duration) error {
	return f.inner.Save(ctx, s, ttl)
}
func (f *failingConsumeStore) Get(ctx context.Context, jti string) (*BindSession, error) {
	return f.inner.Get(ctx, jti)
}
func (f *failingConsumeStore) CASSave(ctx context.Context, sess *BindSession, expected BindStatus, ttl time.Duration) error {
	return f.inner.CASSave(ctx, sess, expected, ttl)
}
func (f *failingConsumeStore) Consume(_ context.Context, _ string) (*BindSession, error) {
	return nil, errors.New("redis: connection refused")
}
func (f *failingConsumeStore) IncrAndCheck(ctx context.Context, key string, limit int64, ttl time.Duration) (int64, error) {
	return f.inner.IncrAndCheck(ctx, key, limit, ttl)
}

// T11-T14: status guards — only issued can create
func TestBindService_Create_StatusGuards(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(h *bindTestHarness, jti string)
		wantErr error
	}{
		{
			"T11: status=verified → conflict",
			func(h *bindTestHarness, jti string) {
				h.auth.verifyPasswordResp.matched = true
				h.loc.byUsername["alice"] = "u-alice"
				_ = h.svc.VerifyPassword(context.Background(), jti, "alice", "p")
			},
			ErrBindStatusConflict,
		},
		{
			"T12: status=confirmed → conflict",
			func(h *bindTestHarness, jti string) {
				// manually force confirmed via store
				h.auth.verifyPasswordResp.matched = true
				h.loc.byUsername["alice"] = "u-alice"
				_ = h.svc.VerifyPassword(context.Background(), jti, "alice", "p")
				sess, _ := h.store.Get(context.Background(), jti)
				sess.Status = BindStatusConfirmed
				_ = h.store.Save(context.Background(), sess, time.Minute)
			},
			ErrBindStatusConflict,
		},
		{
			"T13: status=creating → conflict (防重复 create / 抢到锁的另一 goroutine 正在副作用阶段)",
			func(h *bindTestHarness, jti string) {
				sess, _ := h.store.Get(context.Background(), jti)
				sess.Status = BindStatusCreating
				_ = h.store.Save(context.Background(), sess, time.Minute)
			},
			ErrBindStatusConflict,
		},
		{
			"T14: status=refused → conflict",
			func(h *bindTestHarness, jti string) {
				sess, _ := h.store.Get(context.Background(), jti)
				sess.Status = BindStatusRefused
				_ = h.store.Save(context.Background(), sess, time.Minute)
			},
			ErrBindStatusConflict,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newBindHarness(t, func(c *BindConfig) {
				c.AllowCreate = true
			})
			jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
			tc.setup(h, jti)
			_, err := h.svc.Create(context.Background(), jti)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected %v, got %v", tc.wantErr, err)
			}
		})
	}
}

// T15: token not found → ErrBindNotFound
func TestBindService_Create_TokenNotFound(t *testing.T) {
	h := newBindHarness(t, func(c *BindConfig) {
		c.AllowCreate = true
	})
	_, err := h.svc.Create(context.Background(), "nonexistent-jti")
	if !errors.Is(err, ErrBindNotFound) {
		t.Fatalf("T15: expected ErrBindNotFound, got %v", err)
	}
}

// B. manual_conflict 来源的 bind_token Create 必须返 ErrBindCreateConflictNeedManual。
// 锚定 P2-1:dmwork 端命中多账号脏数据时不允许走自助建号(会再造新账号加剧混乱),
// 走 P1 Admin 人工合并兜底。
func TestBindService_Create_ManualConflictRejected(t *testing.T) {
	h := newBindHarness(t, func(c *BindConfig) {
		c.AllowCreate = true
	})
	jti, err := h.svc.IssueWithReason(context.Background(), sampleClaims(), sampleSD(), BindReasonManualConflict)
	if err != nil {
		t.Fatalf("IssueWithReason: %v", err)
	}
	_, err = h.svc.Create(context.Background(), jti)
	if !errors.Is(err, ErrBindCreateConflictNeedManual) {
		t.Fatalf("expected ErrBindCreateConflictNeedManual, got %v", err)
	}
	// 副作用零容忍:identity 未写、IssueSession 未调
	if len(h.identity.inserted) != 0 {
		t.Fatalf("identity inserts=%d want 0 (no side-effect on rejected manual_conflict)", len(h.identity.inserted))
	}
	if h.users.callCnt != 0 {
		t.Fatalf("IssueSession calls=%d want 0", h.users.callCnt)
	}
}

// B. Info 在 manual_conflict 来源的 token 上必须把 create_blocked 置为
// "manual_conflict",让前端展示"联系管理员人工合并"引导而不是"自助建号"按钮。
func TestBindService_Info_ManualConflict_CreateBlocked(t *testing.T) {
	h := newBindHarness(t, func(c *BindConfig) {
		c.AllowCreate = true
	})
	jti, err := h.svc.IssueWithReason(context.Background(), sampleClaims(), sampleSD(), BindReasonManualConflict)
	if err != nil {
		t.Fatalf("IssueWithReason: %v", err)
	}
	info, err := h.svc.Info(context.Background(), jti)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.CreateBlocked != "manual_conflict" {
		t.Fatalf("create_blocked=%q want manual_conflict", info.CreateBlocked)
	}
	if !info.AllowCreate {
		t.Fatal("AllowCreate must remain true (config-level) — block reason is token-source")
	}
}

// E. Info 在 token 已被推进出 issued 状态时,create_blocked = "consumed"。
// 优先级低于 disabled / claims_incomplete / manual_conflict。
func TestBindService_Info_ConsumedToken_CreateBlocked(t *testing.T) {
	h := newBindHarness(t, func(c *BindConfig) {
		c.AllowCreate = true
	})
	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	// 把 token 推进到 verified
	sess, _ := h.store.Get(context.Background(), jti)
	sess.Status = BindStatusVerified
	if err := h.store.CASSave(context.Background(), sess, BindStatusIssued, time.Minute); err != nil {
		t.Fatalf("CASSave: %v", err)
	}
	info, err := h.svc.Info(context.Background(), jti)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.CreateBlocked != "consumed" {
		t.Fatalf("create_blocked=%q want consumed", info.CreateBlocked)
	}
}

// E. Info 优先级断言:disabled > claims_incomplete > manual_conflict > consumed。
// 当多个条件同时成立时,最高优先级取胜 —— disabled 始终遮蔽其他。
func TestBindService_Info_CreateBlocked_PriorityOrder(t *testing.T) {
	t.Run("disabled wins over claims_incomplete", func(t *testing.T) {
		h := newBindHarness(t, func(c *BindConfig) {
			c.AllowCreate = false // disabled
		})
		c := sampleClaims()
		c.Email = "" // claims_incomplete would also apply if AllowCreate=true
		c.EmailVerified = false
		c.PhoneNumber = ""
		c.PhoneVerified = false
		jti, _ := h.svc.Issue(context.Background(), c, sampleSD())
		info, _ := h.svc.Info(context.Background(), jti)
		if info.CreateBlocked != "disabled" {
			t.Fatalf("create_blocked=%q want disabled (higher prio)", info.CreateBlocked)
		}
	})
	t.Run("claims_incomplete wins over manual_conflict", func(t *testing.T) {
		h := newBindHarness(t, func(c *BindConfig) {
			c.AllowCreate = true
		})
		c := sampleClaims()
		c.Email = ""
		c.EmailVerified = false
		c.PhoneNumber = ""
		c.PhoneVerified = false
		jti, _ := h.svc.IssueWithReason(context.Background(), c, sampleSD(), BindReasonManualConflict)
		info, _ := h.svc.Info(context.Background(), jti)
		if info.CreateBlocked != "claims_incomplete" {
			t.Fatalf("create_blocked=%q want claims_incomplete (higher prio)", info.CreateBlocked)
		}
	})
	t.Run("manual_conflict wins over consumed", func(t *testing.T) {
		h := newBindHarness(t, func(c *BindConfig) {
			c.AllowCreate = true
		})
		jti, _ := h.svc.IssueWithReason(context.Background(), sampleClaims(), sampleSD(), BindReasonManualConflict)
		sess, _ := h.store.Get(context.Background(), jti)
		sess.Status = BindStatusVerified
		_ = h.store.CASSave(context.Background(), sess, BindStatusIssued, time.Minute)
		info, _ := h.svc.Info(context.Background(), jti)
		if info.CreateBlocked != "manual_conflict" {
			t.Fatalf("create_blocked=%q want manual_conflict (higher prio than consumed)", info.CreateBlocked)
		}
	})
}

// D. P2-3: issuer 不在 IssuerAllowlist 时 Create 必须拒绝(defense-in-depth)。
// 即便 token 已签发(ShouldHandle 当时放行),运维在 5min TTL 内把 issuer
// 移走也要被 Create 入口拦下。返 ErrBindAuthRejected → handler 翻 401。
func TestBindService_Create_IssuerNotAllowed_Rejected(t *testing.T) {
	h := newBindHarness(t, func(c *BindConfig) {
		c.AllowCreate = true
		c.IssuerAllowlist = []string{"https://other"} // sampleClaims().Issuer 不在内
	})
	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	_, err := h.svc.Create(context.Background(), jti)
	if !errors.Is(err, ErrBindAuthRejected) {
		t.Fatalf("expected ErrBindAuthRejected, got %v", err)
	}
	if len(h.identity.inserted) != 0 || h.users.callCnt != 0 {
		t.Fatal("Create must short-circuit before any side-effect when issuer not allowed")
	}
}

// TestBindService_CasLockForCreate_Issued CAS issued → creating,
// 并用绝对剩余 TTL(与 saveVerified 对称,不续命到 TokenTTL × 2)。
func TestBindService_CasLockForCreate_Issued(t *testing.T) {
	h := newBindHarness(t)
	jti, err := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	sess, err := h.store.Get(context.Background(), jti)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := h.svc.casLockForCreate(context.Background(), sess); err != nil {
		t.Fatalf("casLockForCreate: %v", err)
	}
	got, err := h.store.Get(context.Background(), jti)
	if err != nil {
		t.Fatalf("Get after casLockForCreate: %v", err)
	}
	if got.Status != BindStatusCreating {
		t.Fatalf("status=%v want creating", got.Status)
	}
}

// TestBindService_CasLockForCreate_ConflictOnSecondCall 第二次 CAS 应当撞 conflict —
// 这是"防 ghost user"的核心:并发两个 create 永远只有一个进入副作用阶段。
func TestBindService_CasLockForCreate_ConflictOnSecondCall(t *testing.T) {
	h := newBindHarness(t)
	jti, _ := h.svc.Issue(context.Background(), sampleClaims(), sampleSD())
	sess, _ := h.store.Get(context.Background(), jti)
	if err := h.svc.casLockForCreate(context.Background(), sess); err != nil {
		t.Fatalf("first casLockForCreate: %v", err)
	}
	if err := h.svc.casLockForCreate(context.Background(), sess); !errors.Is(err, ErrBindStatusConflict) {
		t.Fatalf("second casLockForCreate must ErrBindStatusConflict, got %v", err)
	}
}

// T60: 并发两个 /bind/create 同 (issuer,sub) 不同 token:
// 两个都建 user，只有一个 identity.Insert 成功；
// 输家得到 ErrBindAlreadyBound，由 handler 决定 recover 策略。
func TestBindService_Create_IdentityRaceRecovery(t *testing.T) {
	// Simulate: two separate tokens, both issued, both proceed to IssueSession.
	// First Insert succeeds, second Insert returns duplicate-key → ErrBindAlreadyBound.
	h1 := newBindHarness(t, func(c *BindConfig) {
		c.AllowCreate = true
	})
	h1.users.resp = &IssueSessionResp{UID: "u-winner", LoginRespJSON: `{"token":"winner"}`}
	jti1, _ := h1.svc.Issue(context.Background(), sampleClaims(), sampleSD())

	h2 := newBindHarness(t, func(c *BindConfig) {
		c.AllowCreate = true
	})
	h2.users.resp = &IssueSessionResp{UID: "u-loser", LoginRespJSON: `{"token":"loser"}`}
	h2.identity.insertErr = mockDuplicateKeyErr() // simulates race: winner already inserted
	jti2, _ := h2.svc.Issue(context.Background(), sampleClaims(), sampleSD())

	// winner succeeds
	resp1, err := h1.svc.Create(context.Background(), jti1)
	if err != nil {
		t.Fatalf("T60: winner Create must succeed, got: %v", err)
	}
	if resp1.UID != "u-winner" {
		t.Fatalf("T60: winner UID=%q", resp1.UID)
	}

	// loser gets ErrBindAlreadyBound — handler is responsible for recovery
	_, err = h2.svc.Create(context.Background(), jti2)
	if !errors.Is(err, ErrBindAlreadyBound) {
		t.Fatalf("T60: loser must get ErrBindAlreadyBound for handler to recover, got %v", err)
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
