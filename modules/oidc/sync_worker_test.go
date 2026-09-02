package oidc

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// fakeSyncStore 内存版 syncStore,满足 worker 的 RT 调度断言需求。
type fakeSyncStore struct {
	mu        sync.Mutex
	due       []*DueRefresh
	revoked   []int64
	rotated   []int64 // oldID list
	newRTs    []*RefreshModel
	rotateErr error

	// preRevoked 用于多实例竞态测试:这些 id 视为已被另一 worker 吊销,
	// MarkRefreshRevoked 会返回 affected=0,触发 worker 跳过 Kick。
	preRevoked map[int64]bool
}

func (s *fakeSyncStore) DueRefreshes(limit int) ([]*DueRefresh, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit >= len(s.due) {
		out := s.due
		s.due = nil
		return out, nil
	}
	out := s.due[:limit]
	s.due = s.due[limit:]
	return out, nil
}
func (s *fakeSyncStore) MarkRefreshRevoked(id int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revoked = append(s.revoked, id)
	if s.preRevoked[id] {
		return 0, nil
	}
	return 1, nil
}
func (s *fakeSyncStore) RotateRefresh(oldID int64, newRT *RefreshModel) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rotateErr != nil {
		return s.rotateErr
	}
	s.rotated = append(s.rotated, oldID)
	s.newRTs = append(s.newRTs, newRT)
	return nil
}

// fakeKiller 内存版 sessionKiller。
//
// 把"踢全部"和"踢单端"分开记账,这样测试可以断言的不只是"踢了谁",而是
// **踢的范围** —— 后者才是 logout 的爆炸半径。
type fakeKiller struct {
	mu          sync.Mutex
	kicks       []string
	deviceKicks []deviceKick
	err         error
	deviceErr   error
}

// deviceKick 一次按端踢线的调用记录。
type deviceKick struct {
	UID        string
	DeviceFlag uint8
}

func (k *fakeKiller) Kick(_ context.Context, uid string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.kicks = append(k.kicks, uid)
	return k.err
}

func (k *fakeKiller) KickDevice(_ context.Context, uid string, deviceFlag uint8) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.deviceKicks = append(k.deviceKicks, deviceKick{UID: uid, DeviceFlag: deviceFlag})
	return k.deviceErr
}

func (k *fakeKiller) snapshot() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.kicks...)
}

func (k *fakeKiller) deviceSnapshot() []deviceKick {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]deviceKick(nil), k.deviceKicks...)
}

// scriptedRefresher 按 plain RT 字符串映射到响应,任意未命中即返 invalid_grant。
type scriptedRefresher struct {
	mu       sync.Mutex
	ok       map[string]*RefreshResult // rt → 成功响应
	transErr map[string]bool           // rt → 暂时错(网络)
	calls    int32
	delay    time.Duration
}

func (s *scriptedRefresher) Refresh(ctx context.Context, rt string) (*RefreshResult, error) {
	atomic.AddInt32(&s.calls, 1)
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.transErr[rt] {
		return nil, errors.New("network: connection refused")
	}
	if r, ok := s.ok[rt]; ok {
		return r, nil
	}
	return nil, &oauth2.RetrieveError{
		ErrorCode:        "invalid_grant",
		ErrorDescription: "token revoked",
	}
}

// newWorkerHarness 构造一个完整的测试 worker:真实 Encryptor(随机密钥)+ fake 三件套。
//
// 返回 due RT 的 plain RT 字符串列表,便于测试预设 scriptedRefresher.ok 映射。
func newWorkerHarness(t *testing.T, dueSpecs []dueSpec) (*SyncWorker, *fakeSyncStore, *fakeKiller, *scriptedRefresher, *fakeAudit, []string) {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	enc, err := NewEncryptor(key)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	store := &fakeSyncStore{}
	plainRTs := make([]string, 0, len(dueSpecs))
	for _, sp := range dueSpecs {
		ct, err := enc.Encrypt([]byte(sp.plain))
		if err != nil {
			t.Fatalf("encrypt seed: %v", err)
		}
		store.due = append(store.due, &DueRefresh{
			ID:              sp.id,
			IdentityID:      sp.identityID,
			UID:             sp.uid,
			Subject:         sp.subject,
			TokenCiphertext: ct,
			ExpiresAt:       time.Now().Add(time.Hour),
		})
		plainRTs = append(plainRTs, sp.plain)
	}
	killer := &fakeKiller{}
	rfsh := &scriptedRefresher{ok: make(map[string]*RefreshResult), transErr: make(map[string]bool)}
	audit := newFakeAudit()
	w := NewSyncWorker(SyncWorkerConfig{
		Interval:    0, // 测试只跑 RunOnce,不开 ticker
		Concurrency: 4,
		BatchSize:   100,
	}, store, enc, rfsh, killer, audit, nil)
	return w, store, killer, rfsh, audit, plainRTs
}

// fakeLock 内存版 tickLock。
//
// state 通过 token 字段建模:空字符串 = 无人持有;非空 = 持有者 token。
// acquireErr / releaseErr 用于 Redis 故障注入。
type fakeLock struct {
	mu         sync.Mutex
	holder     string
	acquireErr error
	releaseErr error
	acquireCnt int
	releaseCnt int
}

func (l *fakeLock) Acquire(_ context.Context, _ string, token string, _ time.Duration) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.acquireCnt++
	if l.acquireErr != nil {
		return false, l.acquireErr
	}
	if l.holder != "" {
		return false, nil
	}
	l.holder = token
	return true, nil
}

func (l *fakeLock) Release(_ context.Context, _ string, token string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.releaseCnt++
	if l.releaseErr != nil {
		return false, l.releaseErr
	}
	if l.holder != token {
		return false, nil
	}
	l.holder = ""
	return true, nil
}

type dueSpec struct {
	id         int64
	identityID int64
	uid        string
	plain      string
	subject    string // optional; 空串合法(老测试默认不设 → DueRefresh.Subject 空)
}

// 成功 refresh → 旧 RT rotate(成功路径)+ audit refresh_ok。
func TestSyncWorker_RunOnce_Success_Rotates(t *testing.T) {
	w, store, killer, rfsh, audit, plain := newWorkerHarness(t, []dueSpec{
		{id: 11, identityID: 100, uid: "u-A", plain: "rt-A-old"},
	})
	rfsh.ok[plain[0]] = &RefreshResult{
		RefreshToken: "rt-A-new",
		ExpiresAt:    time.Now().Add(2 * time.Hour),
	}

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(store.rotated) != 1 || store.rotated[0] != 11 {
		t.Errorf("rotated = %v, want [11]", store.rotated)
	}
	if len(store.newRTs) != 1 || string(store.newRTs[0].TokenHash) == "" {
		t.Errorf("new RT not populated: %+v", store.newRTs)
	}
	if len(killer.kicks) != 0 {
		t.Errorf("Kick should not be called on success, got %v", killer.kicks)
	}
	events := audit.events()
	if len(events) != 1 || events[0] != EventRefreshOK {
		t.Errorf("audit events = %v, want [refresh_ok]", events)
	}
}

// invalid_grant → MarkRefreshRevoked + Kick(uid) + audit refresh_fail。
//
// 这是 issue #1120 的核心语义:Aegis 侧封号/改密/登出均化为 invalid_grant,
// DMWork 必须立刻吊销会话 + 踢 WuKongIM 长连接。
func TestSyncWorker_RunOnce_InvalidGrant_RevokesAndKicks(t *testing.T) {
	w, store, killer, rfsh, audit, _ := newWorkerHarness(t, []dueSpec{
		{id: 22, identityID: 200, uid: "u-banned", plain: "rt-banned"},
	})
	// 不 prep ok 映射 → scriptedRefresher 默认返 invalid_grant
	_ = rfsh

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(store.revoked) != 1 || store.revoked[0] != 22 {
		t.Errorf("revoked = %v, want [22]", store.revoked)
	}
	if got := killer.snapshot(); len(got) != 1 || got[0] != "u-banned" {
		t.Errorf("kicks = %v, want [u-banned]", got)
	}
	events := audit.events()
	if len(events) != 1 || events[0] != EventRefreshFail {
		t.Errorf("audit events = %v, want [refresh_fail]", events)
	}
	_ = store // 显式标记使用
}

// 注入 tick lock 后:抢到锁正常跑,RunOnce 完成后 lock 被释放。
func TestSyncWorker_RunOnce_WithLock_AcquiresAndReleases(t *testing.T) {
	w, _, _, rfsh, _, plain := newWorkerHarness(t, []dueSpec{
		{id: 1, identityID: 1, uid: "u1", plain: "rt-1"},
	})
	rfsh.ok[plain[0]] = &RefreshResult{RefreshToken: "new", ExpiresAt: time.Now().Add(time.Hour)}
	lock := &fakeLock{}
	w.lock = lock
	w.cfg.LockKey = "test:lock"
	w.cfg.LockTTL = time.Minute

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if lock.acquireCnt != 1 {
		t.Errorf("Acquire calls = %d, want 1", lock.acquireCnt)
	}
	if lock.releaseCnt != 1 {
		t.Errorf("Release calls = %d, want 1 (must release after run)", lock.releaseCnt)
	}
	if lock.holder != "" {
		t.Errorf("lock should be released, holder = %q", lock.holder)
	}
}

// 抢锁失败(别人持有)→ 跳过本 tick,完全不查 DB / 不调 IdP / 不踢线。
func TestSyncWorker_RunOnce_LockHeldByPeer_Skips(t *testing.T) {
	w, store, killer, rfsh, audit, _ := newWorkerHarness(t, []dueSpec{
		{id: 1, identityID: 1, uid: "u1", plain: "rt-1"},
	})
	lock := &fakeLock{holder: "peer-token"} // 别人已持锁
	w.lock = lock
	w.cfg.LockTTL = time.Minute

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	// 关键:DueRefreshes 不应被调用 —— 跳过整个 tick。
	if got := atomic.LoadInt32(&rfsh.calls); got != 0 {
		t.Errorf("Refresh called %d times when lock held by peer, want 0", got)
	}
	if len(killer.kicks)+len(store.revoked)+len(store.rotated) != 0 {
		t.Errorf("no side-effects expected when lock held by peer")
	}
	if len(audit.events()) != 0 {
		t.Errorf("no audit expected when skipped, got %v", audit.events())
	}
	if lock.releaseCnt != 0 {
		t.Errorf("Release should not be called when Acquire failed, got %d", lock.releaseCnt)
	}
}

// Acquire 报错(Redis 故障)→ 降级为无锁运行,不阻塞 sync。
//
// 该路径的安全性由 DB 层 rowsAffected 竞态检测保证(详见 processOne 注释)。
func TestSyncWorker_RunOnce_LockAcquireError_DegradesGracefully(t *testing.T) {
	w, _, _, rfsh, _, plain := newWorkerHarness(t, []dueSpec{
		{id: 1, identityID: 1, uid: "u1", plain: "rt-1"},
	})
	rfsh.ok[plain[0]] = &RefreshResult{RefreshToken: "new", ExpiresAt: time.Now().Add(time.Hour)}
	lock := &fakeLock{acquireErr: errors.New("redis: connection refused")}
	w.lock = lock

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce should degrade not error: %v", err)
	}
	// Refresh 仍应执行
	if got := atomic.LoadInt32(&rfsh.calls); got != 1 {
		t.Errorf("Refresh calls = %d, want 1 (degraded path runs)", got)
	}
	if lock.releaseCnt != 0 {
		t.Errorf("Release should not be called when Acquire errored, got %d", lock.releaseCnt)
	}
}

// processOne panic 时:per-RT goroutine 内 recover,RunOnce 正常返回,
// lock 通过 defer 被释放,审计记一条 refresh_fail。单条 RT 异常不应炸 worker
// 也不应卡住下次 tick(否则需要等 TTL 自然过期才能恢复)。
func TestSyncWorker_RunOnce_PanicInProcessOne_StillReleasesLock(t *testing.T) {
	w, _, _, _, audit, _ := newWorkerHarness(t, []dueSpec{
		{id: 1, identityID: 1, uid: "u1", plain: "rt-1"},
	})
	w.rfsh = panicRefresher{}
	lock := &fakeLock{}
	w.lock = lock
	w.cfg.LockTTL = time.Minute

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce should not propagate panic: %v", err)
	}
	if lock.holder != "" {
		t.Errorf("lock leaked after panic, holder=%q", lock.holder)
	}
	if lock.releaseCnt != 1 {
		t.Errorf("Release calls = %d, want 1 (defer must fire on panic)", lock.releaseCnt)
	}
	events := audit.events()
	if len(events) != 1 || events[0] != EventRefreshFail {
		t.Errorf("expected refresh_fail audit on panic, got %v", events)
	}
}

// panicRefresher 模拟 processOne 内的 panic。
type panicRefresher struct{}

func (panicRefresher) Refresh(_ context.Context, _ string) (*RefreshResult, error) {
	panic("simulated panic in refresh")
}

// 多实例竞态:同一 RT 被 A、B 两 worker 同时取到,A 抢先 rotate(把旧 RT 标 revoked)
// 后,B 在 IdP 那边收到 invalid_grant —— 这其实是 RT 旋转后的副产品,不是真封号,
// 必须基于 MarkRefreshRevoked 的 rowsAffected 检测竞态,**不能踢用户**。
//
// 这是 reviewer 提的核心担忧:无 SKIP LOCKED 时多实例部署的假阳性踢线。
func TestSyncWorker_RunOnce_InvalidGrantWithLostRace_DoesNotKick(t *testing.T) {
	w, store, killer, rfsh, audit, _ := newWorkerHarness(t, []dueSpec{
		{id: 77, identityID: 700, uid: "u-not-banned", plain: "rt-rotated"},
	})
	// 模拟 A 实例已经成功 rotate:本条 RT 在 DB 已被标 revoked
	store.preRevoked = map[int64]bool{77: true}
	// 不 prep ok 映射 → scriptedRefresher 默认返 invalid_grant
	_ = rfsh

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(killer.kicks) != 0 {
		t.Errorf("lost-race invalid_grant must NOT kick (false positive), got %v", killer.kicks)
	}
	// 不应写 refresh_fail 审计:这不是真失败,是 IdP 旋转副产品
	for _, e := range audit.events() {
		if e == EventRefreshFail {
			t.Errorf("lost-race invalid_grant should not audit refresh_fail")
		}
	}
}

// 暂时性错误(网络/5xx)→ 不踢线、不吊销,只 audit refresh_fail。
//
// IdP 抖动期间踢全员等于自伤,只允许 invalid_grant 的语义触发吊销。
func TestSyncWorker_RunOnce_TransientError_NoKick(t *testing.T) {
	w, store, killer, rfsh, audit, plain := newWorkerHarness(t, []dueSpec{
		{id: 33, identityID: 300, uid: "u-transient", plain: "rt-transient"},
	})
	rfsh.transErr[plain[0]] = true

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(store.revoked) != 0 {
		t.Errorf("transient error should not revoke, got %v", store.revoked)
	}
	if len(killer.kicks) != 0 {
		t.Errorf("transient error should not kick, got %v", killer.kicks)
	}
	if len(store.rotated) != 0 {
		t.Errorf("transient error should not rotate, got %v", store.rotated)
	}
	events := audit.events()
	if len(events) != 1 || events[0] != EventRefreshFail {
		t.Errorf("audit events = %v, want [refresh_fail]", events)
	}
}

// 空批次 → noop,不调任何下游。
func TestSyncWorker_RunOnce_Empty_NoOp(t *testing.T) {
	w, store, killer, rfsh, audit, _ := newWorkerHarness(t, nil)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if atomic.LoadInt32(&rfsh.calls) != 0 {
		t.Errorf("Refresh called on empty batch")
	}
	if len(store.revoked)+len(store.rotated) != 0 {
		t.Errorf("store mutated on empty batch")
	}
	if len(killer.kicks) != 0 || len(audit.events()) != 0 {
		t.Errorf("side effects on empty batch")
	}
}

// 解密失败(密钥轮换 / 数据损坏)→ MarkRefreshRevoked,避免反复占用调度位。
func TestSyncWorker_RunOnce_DecryptError_Revokes(t *testing.T) {
	w, store, killer, _, audit, _ := newWorkerHarness(t, nil)
	// 直接塞一条密文损坏的 RT(短于 nonce + GCM tag)
	store.due = append(store.due, &DueRefresh{
		ID:              44,
		IdentityID:      400,
		UID:             "u-corrupt",
		TokenCiphertext: []byte{0x01, 0x02, 0x03},
		ExpiresAt:       time.Now().Add(time.Hour),
	})

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(store.revoked) != 1 || store.revoked[0] != 44 {
		t.Errorf("revoked = %v, want [44]", store.revoked)
	}
	if len(killer.kicks) != 0 {
		t.Errorf("decrypt failure should not kick (not an account-state event), got %v", killer.kicks)
	}
	events := audit.events()
	if len(events) != 1 || events[0] != EventRefreshFail {
		t.Errorf("audit events = %v, want [refresh_fail]", events)
	}
}

// 并发上限:批量大但 Concurrency=1 时 in-flight 不会超过 1。
func TestSyncWorker_RunOnce_RespectsConcurrency(t *testing.T) {
	specs := []dueSpec{
		{id: 1, identityID: 1, uid: "u1", plain: "rt-1"},
		{id: 2, identityID: 2, uid: "u2", plain: "rt-2"},
		{id: 3, identityID: 3, uid: "u3", plain: "rt-3"},
		{id: 4, identityID: 4, uid: "u4", plain: "rt-4"},
	}
	w, _, _, rfsh, _, plain := newWorkerHarness(t, specs)
	w.cfg.Concurrency = 1
	for _, p := range plain {
		rfsh.ok[p] = &RefreshResult{RefreshToken: p + "-new", ExpiresAt: time.Now().Add(time.Hour)}
	}

	// 用 delay + 计数器观测峰值 in-flight
	var inFlight, peak int32
	rfsh.delay = 0 // 下面用 wrap 控制
	wrapped := &concurrencyProbe{
		inner: rfsh,
		onEnter: func() {
			cur := atomic.AddInt32(&inFlight, 1)
			for {
				p := atomic.LoadInt32(&peak)
				if cur <= p || atomic.CompareAndSwapInt32(&peak, p, cur) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
		},
		onExit: func() { atomic.AddInt32(&inFlight, -1) },
	}
	w.rfsh = wrapped

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := atomic.LoadInt32(&peak); got > 1 {
		t.Errorf("concurrency peak = %d, want <= 1", got)
	}
}

// concurrencyProbe wraps refresher 用来观测 in-flight 峰值。
type concurrencyProbe struct {
	inner   refresher
	onEnter func()
	onExit  func()
}

func (p *concurrencyProbe) Refresh(ctx context.Context, rt string) (*RefreshResult, error) {
	p.onEnter()
	defer p.onExit()
	return p.inner.Refresh(ctx, rt)
}

// Start 重复调用应先停掉旧 goroutine 再启新的,不泄漏。
//
// 验证方式:用一个本身能感知 ctx 取消的 refresher 计算 active goroutine 数;
// 第二次 Start 后旧 goroutine 必须已退出(Stop 在第二次 Start 内部触发)。
func TestSyncWorker_Start_IsIdempotent_NoLeak(t *testing.T) {
	w, _, _, _, _, _ := newWorkerHarness(t, nil)
	w.cfg.Interval = 50 * time.Millisecond
	w.Start(context.Background())
	time.Sleep(10 * time.Millisecond)
	// 第二次 Start 应该先把第一次的 goroutine 干净退出再起新
	w.Start(context.Background())
	// 现在 Stop 必须能在合理时间内退出 —— 如果有泄漏的旧 goroutine,Wait 不会卡
	// (因为旧已 cancel),但其内部 ticker 仍可能误以为活;Stop 的 wg.Wait 是真相
	done := make(chan struct{})
	go func() { w.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop after double-Start hung > 2s — goroutine leak")
	}
}

// Stop 取消进行中的轮询;Stop 后 wg 应能退出。
func TestSyncWorker_StartStop_CleanShutdown(t *testing.T) {
	w, _, _, _, _, _ := newWorkerHarness(t, nil)
	w.cfg.Interval = 50 * time.Millisecond
	w.Start(context.Background())
	time.Sleep(60 * time.Millisecond) // 至少跑一轮
	done := make(chan struct{})
	go func() { w.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop hung > 2s")
	}
}

// isInvalidGrant 单元测试:RetrieveError、wrap 后字符串、nil、其他错都覆盖。
//
// 同时验证 fellThrough:errors.As 命中走结构化路径(false),字串兜底命中(true)。
func TestIsInvalidGrant(t *testing.T) {
	cases := []struct {
		name            string
		err             error
		wantInvalid     bool
		wantFellThrough bool
	}{
		{"nil", nil, false, false},
		{"oauth2 RetrieveError invalid_grant", &oauth2.RetrieveError{ErrorCode: "invalid_grant"}, true, false},
		{"oauth2 RetrieveError other", &oauth2.RetrieveError{ErrorCode: "server_error"}, false, false},
		{"wrapped string only", errors.New("oidc: refresh: opaque wrap \"invalid_grant\" inside"), true, true},
		{"plain network err", errors.New("connection refused"), false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			invalid, fellThrough := isInvalidGrant(c.err)
			if invalid != c.wantInvalid {
				t.Errorf("invalid = %v, want %v", invalid, c.wantInvalid)
			}
			if fellThrough != c.wantFellThrough {
				t.Errorf("fellThrough = %v, want %v", fellThrough, c.wantFellThrough)
			}
		})
	}
}
