package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// BindStore bind_token 状态 + 限流计数器的 Redis 抽象。
//
// 与 StateStore 平行设计:生产用 Redis,测试用 memory 实现。两条实现必须
// 在 bind_store_test.go 的同一组表驱动测试下通过。
//
// 接口职责拆分(每个方法只做一件事):
//   - Save / Get / Consume:bind_token 整体生命周期(签发 → 读取 → 终态消费)
//   - CASSave:状态机 CAS 迁移 —— 期望 Status 匹配时原子写入整段 sess
//   - IncrAndCheck:任意维度的限流计数器(SR-2.1 bind_token 维度 + SR-2.2 uid 维度)
//
// 一次性消费(SR-1)由 Consume 保证 —— 取出立即 DEL,即便后续 confirm 失败
// 也不可重放。状态机 CAS 是次要防护层(防 verified→confirmed 并发重入,
// 以及 issued→verified 并发 verify 覆盖 CandidateUID 的身份接管)。
type BindStore interface {
	// Save 写入新 session,TTL 由 cfg 控制(NFR-2 默认 5min)。
	// session.JTI 非空,Status 调用方指定(通常 BindStatusIssued)。
	//
	// **不做 CAS** —— Save 只在 Issue 阶段(首次写入)使用。状态机后续
	// 迁移必须走 CASSave,否则两个并发 verify 可同时观察到 status=issued,
	// 双 Save 后第二个覆盖第一个的 CandidateUID(身份接管)。
	Save(ctx context.Context, s *BindSession, ttl time.Duration) error

	// Get 读快照不消费。verify / info 路径用,允许多次读。
	// key 不存在返 ErrBindNotFound。
	Get(ctx context.Context, jti string) (*BindSession, error)

	// CASSave 原子地校验 Redis 里当前 sess.Status 与 expected 一致后,
	// 写入入参 sess 的完整 payload 并刷新 TTL。
	//
	// 与朴素 Save 的区别:在 Redis 单线程里完成 "check status → write payload",
	// 两个并发调用即便在客户端侧都读到了相同的旧快照,只有一个能成功 ——
	// 第二个会因 status 已被推进到新值而返 ErrBindStatusConflict。
	//
	// 调用方:bind_service.saveVerified(expected=issued),将来 verified→
	// confirmed 也走这条路径。key 不存在返 ErrBindNotFound。
	CASSave(ctx context.Context, sess *BindSession, expected BindStatus, ttl time.Duration) error

	// Consume 取出并立即删除,confirm 成功路径调(SR-1 单次消费)。
	// key 不存在返 ErrBindNotFound。
	Consume(ctx context.Context, jti string) (*BindSession, error)

	// IncrAndCheck 通用计数器 +1 → 与 limit 比较 → 超返 ErrBindRateLimited。
	// key 由调用方按维度拼装(如 "bind:verify:"+jti / "bind:uidfail:"+uid),
	// 避免 BindStore 持有维度知识。ttl 是首次 +1 设的窗口长度,后续 +1 沿用。
	//
	// 返回的 int64 是 +1 后的当前计数,供审计/告警按值定级。
	IncrAndCheck(ctx context.Context, key string, limit int64, ttl time.Duration) (int64, error)
}

// 哨兵错误。调用方按类型而非字符串判断,避免文案改动影响业务路径。
var (
	ErrBindNotFound       = errors.New("oidc: bind session not found or expired")
	ErrBindStatusConflict = errors.New("oidc: bind status transition conflict")
	ErrBindRateLimited    = errors.New("oidc: bind rate limit exceeded")
	// ErrBindNoPhone claims 里没有可信手机号(空 / phone_verified=false),
	// 短信路径(SendSMS / VerifySMS)不可用 —— FR-3.3。
	// 与"SMSService 内部失败"区分,让 handler 把前者翻 400(业务前提不满足,
	// 客户端不应当 retry),后者翻 500(基础设施异常,可重试)。
	ErrBindNoPhone = errors.New("oidc: bind claims has no verified phone")
	// ErrBindConflictNeedManual claims 命中多条 dmwork user(同 phone 多账号),
	// 自助流程无法判定,走 P1 Admin 人工兜底。
	ErrBindConflictNeedManual = errors.New("oidc: bind claims match multiple dmwork users")
	// ErrBindAlreadyBound 同 (issuer, sub) 已绑到别的 uid;或同 (uid, issuer)
	// 已存在另一 sub。两种都触发 DB 唯一约束 → 1062,confirm 路径需要分别
	// 翻成对用户更友好的 409。
	ErrBindAlreadyBound = errors.New("oidc: bind identity already exists")
	// ErrBindAuthRejected 用户输入(密码/OTP)在底层校验被拒绝。与"内部错误"
	// 区分,让 metric label 能正确归到 unauthorized 而非 internal_error。
	// service 层用它包装 auth.* 的 matched=false / VerifyOIDCBindSMS 拒绝。
	ErrBindAuthRejected = errors.New("oidc: bind auth rejected")
	// ErrBindMethodDisabled 调用方请求的方法被运维通过 OCTO_OIDC_BIND_METHODS
	// 关闭了。Methods 必须是真实策略,不仅 UI 过滤 —— 否则攻击者可以硬调
	// 端点绕过运维"禁用密码"的安全开关。handler 翻 400,与"参数非法"同档,
	// 不属于身份凭据拒绝,因此与 ErrBindAuthRejected 区分。
	ErrBindMethodDisabled = errors.New("oidc: bind method disabled by configuration")

	// ErrBindCreateClaimsIncomplete claims 既无 verified email 也无 verified phone:
	// /bind/create 拒绝建号,因为后续客服 / 找回流程没有可信账号锚点。
	// handler 翻 422,metric label claims_incomplete。
	ErrBindCreateClaimsIncomplete = errors.New("oidc bind: claims missing required fields")

	// ErrBindCreateConflictNeedManual bind_token 是从 manual-conflict 来源签发的
	// (claims 命中多条 dmwork 账号);Create 路径拒绝建号,只能走 P1 Admin 人工合并。
	// handler 翻 409,metric label conflict_need_manual。与 ErrBindConflictNeedManual
	// (verify 路径短信多匹配)语义平行,但 token 维度的拒绝在 Create 入口落地,
	// 与 Issue 阶段固化的 IssueReason 字段联动。
	ErrBindCreateConflictNeedManual = errors.New("oidc bind: create rejected: claims conflict needs manual resolution")
)

// ---------- memory impl (单测 + 本地开发) ----------
//
// memory impl 含简单的"取时回收过期"逻辑(类似 memoryStateStore),
// 不跑后台 GC,长期运行场景需自行清理。

type memoryBindStore struct {
	mu       sync.Mutex
	sessions map[string]memoryBindEntry
	counters map[string]memoryCounterEntry
}

type memoryBindEntry struct {
	data      []byte // BindSession 的 JSON,与 redis 实现编码格式一致
	expiresAt time.Time
}

type memoryCounterEntry struct {
	count     int64
	expiresAt time.Time
}

func newMemoryBindStore() *memoryBindStore {
	return &memoryBindStore{
		sessions: make(map[string]memoryBindEntry),
		counters: make(map[string]memoryCounterEntry),
	}
}

func (m *memoryBindStore) Save(_ context.Context, s *BindSession, ttl time.Duration) error {
	if s == nil || s.JTI == "" {
		return errors.New("oidc: bind session: jti required")
	}
	if ttl <= 0 {
		return fmt.Errorf("oidc: bind session: ttl must be positive, got %v", ttl)
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("oidc: bind session marshal: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[s.JTI] = memoryBindEntry{data: encoded, expiresAt: time.Now().Add(ttl)}
	return nil
}

func (m *memoryBindStore) Get(_ context.Context, jti string) (*BindSession, error) {
	if jti == "" {
		return nil, ErrBindNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.sessions[jti]
	if !ok || time.Now().After(entry.expiresAt) {
		delete(m.sessions, jti)
		return nil, ErrBindNotFound
	}
	return decodeBindSession(entry.data)
}

func (m *memoryBindStore) CASSave(_ context.Context, sess *BindSession, expected BindStatus, ttl time.Duration) error {
	if sess == nil || sess.JTI == "" {
		return errors.New("oidc: bind CASSave: sess/jti required")
	}
	if ttl <= 0 {
		return fmt.Errorf("oidc: bind CASSave: ttl must be positive, got %v", ttl)
	}
	encoded, err := json.Marshal(sess)
	if err != nil {
		return fmt.Errorf("oidc: bind session marshal: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.sessions[sess.JTI]
	if !ok || time.Now().After(entry.expiresAt) {
		delete(m.sessions, sess.JTI)
		return ErrBindNotFound
	}
	current, err := decodeBindSession(entry.data)
	if err != nil {
		return err
	}
	// 在锁内比对当前 status 与 expected:即使两个并发调用都基于同一旧快照
	// 修改了 sess.Status,只有一个能拿到 expected==current.Status,另一个
	// 看到的是已被推进后的状态 → ErrBindStatusConflict。
	if current.Status != expected {
		return ErrBindStatusConflict
	}
	m.sessions[sess.JTI] = memoryBindEntry{data: encoded, expiresAt: time.Now().Add(ttl)}
	return nil
}

func (m *memoryBindStore) Consume(_ context.Context, jti string) (*BindSession, error) {
	if jti == "" {
		return nil, ErrBindNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.sessions[jti]
	if !ok || time.Now().After(entry.expiresAt) {
		delete(m.sessions, jti)
		return nil, ErrBindNotFound
	}
	delete(m.sessions, jti)
	return decodeBindSession(entry.data)
}

func (m *memoryBindStore) IncrAndCheck(_ context.Context, key string, limit int64, ttl time.Duration) (int64, error) {
	if key == "" {
		return 0, errors.New("oidc: bind IncrAndCheck: key required")
	}
	if limit <= 0 {
		return 0, fmt.Errorf("oidc: bind IncrAndCheck: limit must be positive, got %d", limit)
	}
	if ttl <= 0 {
		return 0, fmt.Errorf("oidc: bind IncrAndCheck: ttl must be positive, got %v", ttl)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	entry, ok := m.counters[key]
	if !ok || now.After(entry.expiresAt) {
		entry = memoryCounterEntry{count: 0, expiresAt: now.Add(ttl)}
	}
	entry.count++
	m.counters[key] = entry
	if entry.count > limit {
		return entry.count, ErrBindRateLimited
	}
	return entry.count, nil
}

func decodeBindSession(b []byte) (*BindSession, error) {
	var s BindSession
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("oidc: bind session unmarshal: %w", err)
	}
	return &s, nil
}
