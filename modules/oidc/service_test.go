package oidc

import (
	"context"
	"errors"
	"testing"

	mysql "github.com/go-sql-driver/mysql"
)

// fakeUserLookup 实现 service 内部依赖的 userLookup 接口,
// 用 in-memory 数据替代 user 模块,聚焦 oidc service 层逻辑。
type fakeUserLookup struct {
	usersByEmail map[string][]string // email -> uids
	usersByPhone map[string][]string // zone+phone -> uids
	loginCalls   []IssueSessionReq
	loginResp    *IssueSessionResp
	loginErr     error

	// loginRespByUID 按请求里的 uid 返回不同会话,优先于 loginResp。
	//
	// identity 竞态测试必须区分两次签发:输家(ghost)与赢家。单一 loginResp
	// 会让两次返回同一个对象,于是"把赢家会话交出去"和"把 ghost 会话交出去"
	// 在断言上不可分 —— 那正是要测的缺陷。
	loginRespByUID map[string]*IssueSessionResp
}

func (f *fakeUserLookup) UIDsByEmail(email string) ([]string, error) {
	return f.usersByEmail[email], nil
}
func (f *fakeUserLookup) UIDsByPhone(zone, phone string) ([]string, error) {
	return f.usersByPhone[zone+"|"+phone], nil
}
func (f *fakeUserLookup) IssueSession(ctx context.Context, req IssueSessionReq) (*IssueSessionResp, error) {
	f.loginCalls = append(f.loginCalls, req)
	if f.loginErr != nil {
		return nil, f.loginErr
	}
	if resp, ok := f.loginRespByUID[req.UID]; ok {
		return resp, nil
	}
	if f.loginResp != nil {
		return f.loginResp, nil
	}
	return &IssueSessionResp{UID: req.UID, IsNewUser: req.CreateUser}, nil
}

// fakeIdentityStore 替代 oidc DB,保留 issuer+sub→uid 映射。
type fakeIdentityStore struct {
	bindings map[string]*IdentityModel // key="<issuer>|<sub>"
	written  []*IdentityModel

	// 故障注入(竞态恢复测试用)
	failInsertWithDuplicate bool // Insert 直接返 MySQL 1062 模拟 unique 冲突
	failGetAfterDuplicate   bool // 后续 Get 返错,模拟查赢家也失败

	// winnerAppearsAfterFirstGet 建模真实的并发交错:赢家那行是在**我方 Get
	// 之后、Insert 之前**落库的。所以第一次 Get(ResolveOrLink 判 IsNew 用的
	// 那次)必须查不到,之后的 Get(recoverFromIdentityRace 找赢家用的那次)
	// 才能查到。
	//
	// 用一个"第几次调用"的开关而不是"永久隐藏",是因为竞态本身就是时序现象:
	// 永久隐藏会让 recoverFromIdentityRace 也找不到赢家,测到的是另一条分支。
	winnerAppearsAfterFirstGet bool
	getCalls                   int
}

func newFakeIdentityStore() *fakeIdentityStore {
	return &fakeIdentityStore{bindings: make(map[string]*IdentityModel)}
}

func (s *fakeIdentityStore) Get(issuer, sub string) (*IdentityModel, error) {
	if s.failGetAfterDuplicate {
		return nil, errors.New("fake DB get failed")
	}
	s.getCalls++
	if s.winnerAppearsAfterFirstGet && s.getCalls == 1 {
		// 赢家还没 commit —— 我方于是判定 IsNew 并去 Insert。
		return nil, nil
	}
	return s.bindings[issuer+"|"+sub], nil
}
func (s *fakeIdentityStore) Insert(m *IdentityModel) error {
	if s.failInsertWithDuplicate {
		// 复刻 go-sql-driver/mysql 的 *MySQLError{Number: 1062}
		return &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}
	}
	s.bindings[m.Issuer+"|"+m.Subject] = m
	s.written = append(s.written, m)
	return nil
}
func (s *fakeIdentityStore) UpdateLogin(id int64, email string, emailVerified int, phone string, phoneVerified int) error {
	return nil
}

func defaultProviderCfg() ProviderConfig {
	return ProviderConfig{
		AutoLinkByEmail: true,
		AutoLinkByPhone: true,
		AllowNewUser:    true,
	}
}

// Cycle 7 RED: 已存在 (issuer, sub) 绑定时,直接返回原 uid 且不创建新用户。
func TestService_ResolveOrLink_ExistingBinding(t *testing.T) {
	store := newFakeIdentityStore()
	_ = store.Insert(&IdentityModel{
		UID:     "u-existing-1",
		Issuer:  "https://aegis",
		Subject: "sub-001",
	})
	users := &fakeUserLookup{}
	svc := newService(defaultProviderCfg(), store, users)

	res, err := svc.ResolveOrLink(context.Background(), &IDTokenClaims{
		Issuer:  "https://aegis",
		Subject: "sub-001",
		Email:   "alice@example.com",
	})
	if err != nil {
		t.Fatalf("ResolveOrLink: %v", err)
	}
	if res.UID != "u-existing-1" {
		t.Errorf("UID = %q, want u-existing-1", res.UID)
	}
	if res.IsNew {
		t.Error("IsNew should be false for existing binding")
	}
	if got := len(store.written); got != 1 {
		t.Errorf("identity should not be re-written, got %d writes", got)
	}
}

// Cycle 8 RED: 未绑定 + 邮箱无匹配 + AllowNewUser=true → 返回 IsNew=true,UID 由 IssueSession 填。
func TestService_ResolveOrLink_NewUser(t *testing.T) {
	store := newFakeIdentityStore()
	users := &fakeUserLookup{} // 空 user 库
	cfg := defaultProviderCfg()
	cfg.AllowNewUser = true
	svc := newService(cfg, store, users)

	res, err := svc.ResolveOrLink(context.Background(), &IDTokenClaims{
		Issuer:        "https://aegis",
		Subject:       "sub-new",
		Email:         "newcomer@example.com",
		EmailVerified: true,
	})
	if err != nil {
		t.Fatalf("ResolveOrLink: %v", err)
	}
	if !res.IsNew {
		t.Error("expected IsNew=true for fresh claim")
	}
	if res.UID != "" {
		t.Errorf("UID should be empty before IssueSession, got %q", res.UID)
	}
	// identity 行此时不应写入(由 callback 拿到 IssueSession 返回的 uid 后再补)
	if got := len(store.written); got != 0 {
		t.Errorf("identity should not be written for new user, got %d writes", got)
	}
}

// Cycle 9 RED: AllowNewUser=false + 无匹配 → ErrUnknownUser。
func TestService_ResolveOrLink_UnknownUser(t *testing.T) {
	store := newFakeIdentityStore()
	users := &fakeUserLookup{}
	cfg := defaultProviderCfg()
	cfg.AllowNewUser = false
	svc := newService(cfg, store, users)

	_, err := svc.ResolveOrLink(context.Background(), &IDTokenClaims{
		Issuer:        "https://aegis",
		Subject:       "sub-stranger",
		Email:         "stranger@example.com",
		EmailVerified: true,
	})
	if !errors.Is(err, ErrUnknownUser) {
		t.Fatalf("expected ErrUnknownUser, got %v", err)
	}
}

// Cycle 10 RED: 邮箱命中唯一历史用户 + AutoLinkByEmail=true → 写绑定 + 返回该 uid。
func TestService_ResolveOrLink_AutoLinkByEmail(t *testing.T) {
	store := newFakeIdentityStore()
	users := &fakeUserLookup{
		usersByEmail: map[string][]string{
			"alice@example.com": {"u-alice"},
		},
	}
	cfg := defaultProviderCfg()
	cfg.AutoLinkByEmail = true
	cfg.RequireEmailVerified = true
	svc := newService(cfg, store, users)

	res, err := svc.ResolveOrLink(context.Background(), &IDTokenClaims{
		Issuer:        "https://aegis",
		Subject:       "sub-001",
		Email:         "alice@example.com",
		EmailVerified: true,
	})
	if err != nil {
		t.Fatalf("ResolveOrLink: %v", err)
	}
	if res.UID != "u-alice" {
		t.Errorf("UID = %q, want u-alice", res.UID)
	}
	if res.IsNew {
		t.Error("IsNew should be false for auto-linked user")
	}
	if got := len(store.written); got != 1 {
		t.Fatalf("expected 1 binding write, got %d", got)
	}
	w := store.written[0]
	if w.UID != "u-alice" || w.Subject != "sub-001" || w.Email != "alice@example.com" {
		t.Errorf("binding fields wrong: %+v", w)
	}
}

// Cycle 11 RED: 邮箱命中多条 → ErrConflictNeedManual,不写绑定。
func TestService_ResolveOrLink_ConflictMultiMatch(t *testing.T) {
	store := newFakeIdentityStore()
	users := &fakeUserLookup{
		usersByEmail: map[string][]string{
			"shared@example.com": {"u-1", "u-2"},
		},
	}
	cfg := defaultProviderCfg()
	svc := newService(cfg, store, users)

	_, err := svc.ResolveOrLink(context.Background(), &IDTokenClaims{
		Issuer:        "https://aegis",
		Subject:       "sub-conflict",
		Email:         "shared@example.com",
		EmailVerified: true,
	})
	if !errors.Is(err, ErrConflictNeedManual) {
		t.Fatalf("expected ErrConflictNeedManual, got %v", err)
	}
	if got := len(store.written); got != 0 {
		t.Errorf("no identity should be written on conflict, got %d", got)
	}
}

// 邮箱未验证 + RequireEmailVerified=true + AllowNewUser=true → 跳过邮箱绑定,
// 走到 step 4 新建用户。不再返回 ErrEmailNotVerified(会短路整条矩阵)。
func TestService_ResolveOrLink_EmailNotVerified(t *testing.T) {
	store := newFakeIdentityStore()
	users := &fakeUserLookup{
		usersByEmail: map[string][]string{
			"alice@example.com": {"u-alice"},
		},
	}
	cfg := defaultProviderCfg()
	cfg.RequireEmailVerified = true
	cfg.AllowNewUser = true
	svc := newService(cfg, store, users)

	res, err := svc.ResolveOrLink(context.Background(), &IDTokenClaims{
		Issuer:        "https://aegis",
		Subject:       "sub-001",
		Email:         "alice@example.com",
		EmailVerified: false,
	})
	if err != nil {
		t.Fatalf("should skip email branch, got err: %v", err)
	}
	if !res.IsNew {
		t.Error("should fall through to AllowNewUser, got IsNew=false")
	}
}

// Cycle 12 RED: IssueSession 透传 req 到 userLookup,带回 LoginRespJSON。
func TestService_IssueSession_Delegates(t *testing.T) {
	users := &fakeUserLookup{
		loginResp: &IssueSessionResp{
			UID:           "u-issued",
			IsNewUser:     false,
			LoginRespJSON: `{"token":"abc","uid":"u-issued"}`,
		},
	}
	svc := newService(defaultProviderCfg(), newFakeIdentityStore(), users)

	resp, err := svc.IssueSession(context.Background(), IssueSessionReq{
		UID:        "u-existing",
		CreateUser: false,
	})
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	if resp.UID != "u-issued" {
		t.Errorf("uid = %q, want u-issued", resp.UID)
	}
	if resp.LoginRespJSON == "" {
		t.Error("LoginRespJSON should pass through")
	}
	if got := len(users.loginCalls); got != 1 {
		t.Fatalf("expected 1 IssueSession call, got %d", got)
	}
	if c := users.loginCalls[0]; c.UID != "u-existing" || c.CreateUser {
		t.Errorf("call mismatch: %+v", c)
	}
}

// 没有 UID 且不创建用户应直接报错(防止误传空 UID 走到 user 模块)。
func TestService_IssueSession_RejectEmptyUIDForExisting(t *testing.T) {
	svc := newService(defaultProviderCfg(), newFakeIdentityStore(), &fakeUserLookup{})
	_, err := svc.IssueSession(context.Background(), IssueSessionReq{CreateUser: false})
	if err == nil {
		t.Fatal("expected error")
	}
}

// 邮箱未验证不应短路整条矩阵:手机号可绑时仍应命中 step 3。
func TestService_ResolveOrLink_EmailUnverifiedFallsToPhone(t *testing.T) {
	store := newFakeIdentityStore()
	users := &fakeUserLookup{
		usersByEmail: map[string][]string{"alice@x.com": {"u-a"}},
		usersByPhone: map[string][]string{"0086|13900000001": {"u-p"}},
	}
	cfg := defaultProviderCfg()
	cfg.RequireEmailVerified = true
	cfg.AutoLinkByEmail = true
	cfg.AutoLinkByPhone = true
	svc := newService(cfg, store, users)

	res, err := svc.ResolveOrLink(context.Background(), &IDTokenClaims{
		Issuer:        "https://aegis",
		Subject:       "sub-fallthru",
		Email:         "alice@x.com",
		EmailVerified: false,
		PhoneNumber:   "+8613900000001",
		PhoneVerified: true,
	})
	if err != nil {
		t.Fatalf("should fall through to phone, got err: %v", err)
	}
	if res.UID != "u-p" {
		t.Errorf("UID = %q, want u-p (phone match)", res.UID)
	}
}

// 邮箱未验证 + 手机也无匹配 + AllowNewUser=true → 应走新建,不应返 ErrEmailNotVerified。
func TestService_ResolveOrLink_EmailUnverifiedFallsToNewUser(t *testing.T) {
	store := newFakeIdentityStore()
	users := &fakeUserLookup{}
	cfg := defaultProviderCfg()
	cfg.RequireEmailVerified = true
	cfg.AllowNewUser = true
	svc := newService(cfg, store, users)

	res, err := svc.ResolveOrLink(context.Background(), &IDTokenClaims{
		Issuer:        "https://aegis",
		Subject:       "sub-newuser",
		Email:         "unverified@x.com",
		EmailVerified: false,
	})
	if err != nil {
		t.Fatalf("should allow new user, got err: %v", err)
	}
	if !res.IsNew {
		t.Error("expected IsNew=true")
	}
}

// AutoLinkByEmail=false 时邮箱不自动绑,但 AutoLinkByPhone=true 时手机号仍可绑。
// 验证两个 flag 独立生效,不会因 email 关掉而连带禁用 phone。
func TestService_ResolveOrLink_PhoneIndependentOfEmailFlag(t *testing.T) {
	store := newFakeIdentityStore()
	users := &fakeUserLookup{
		usersByPhone: map[string][]string{
			"0086|13900000001": {"u-phone"},
		},
	}
	cfg := defaultProviderCfg()
	cfg.AutoLinkByEmail = false
	cfg.AutoLinkByPhone = true
	svc := newService(cfg, store, users)

	res, err := svc.ResolveOrLink(context.Background(), &IDTokenClaims{
		Issuer:        "https://aegis",
		Subject:       "sub-p",
		PhoneNumber:   "+8613900000001",
		PhoneVerified: true,
	})
	if err != nil {
		t.Fatalf("ResolveOrLink: %v", err)
	}
	if res.UID != "u-phone" {
		t.Errorf("UID = %q, want u-phone", res.UID)
	}
}
