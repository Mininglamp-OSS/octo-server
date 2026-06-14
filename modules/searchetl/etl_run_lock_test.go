package searchetl

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeLock 是 runLock 的可控假实现，用于验证 C3 锁编排语义（无需真实 Redis）。
type fakeLock struct {
	acquireOK  bool  // Acquire 是否抢到
	acquireErr error // Acquire 是否 Redis 故障

	renewErr   error // Renew 是否报错
	renewFalse bool  // Renew 是否返回 false（owner 丢失）
	renewAfter int   // 前 N 次 Renew 正常，第 N+1 次起按 renewErr/renewFalse 失败

	acquireCalls atomic.Int32
	renewCalls   atomic.Int32
	releaseCalls atomic.Int32
	released     atomic.Bool
}

func (f *fakeLock) Acquire(token string) (bool, error) {
	f.acquireCalls.Add(1)
	if f.acquireErr != nil {
		return false, f.acquireErr
	}
	return f.acquireOK, nil
}

func (f *fakeLock) Renew(token string) (bool, error) {
	n := int(f.renewCalls.Add(1))
	if n <= f.renewAfter {
		return true, nil // 前 renewAfter 次正常续租
	}
	if f.renewErr != nil {
		return false, f.renewErr
	}
	if f.renewFalse {
		return false, nil
	}
	return true, nil
}

func (f *fakeLock) Release(token string) error {
	f.releaseCalls.Add(1)
	f.released.Store(true)
	return nil
}

// TestRunLockedTick_AcquireFailsSkips 抢不到锁（别副本在跑）→ 跳过本轮返回 nil，run 不执行。
func TestRunLockedTick_AcquireFailsSkips(t *testing.T) {
	lock := &fakeLock{acquireOK: false}
	ran := false
	err := runLockedTick(context.Background(), lock, time.Hour, lg(), func(ctx context.Context) error {
		ran = true
		return nil
	})
	if err != nil {
		t.Fatalf("acquire-miss must return nil, got %v", err)
	}
	if ran {
		t.Fatalf("run must NOT execute when lock not acquired (no duplicate work)")
	}
	if lock.releaseCalls.Load() != 0 {
		t.Fatalf("must not release a lock we never acquired")
	}
}

// TestRunLockedTick_RedisErrorSkips Redis 故障 → 不裸跑（避免多副本同跑），返回 err，run 不执行。
func TestRunLockedTick_RedisErrorSkips(t *testing.T) {
	lock := &fakeLock{acquireErr: errors.New("redis down")}
	ran := false
	err := runLockedTick(context.Background(), lock, time.Hour, lg(), func(ctx context.Context) error {
		ran = true
		return nil
	})
	if err == nil {
		t.Fatalf("redis error should propagate")
	}
	if ran {
		t.Fatalf("run must NOT execute on redis error")
	}
}

// TestRunLockedTick_HappyPathReleases 抢到锁 → run 执行 → 结束释放锁。
func TestRunLockedTick_HappyPathReleases(t *testing.T) {
	lock := &fakeLock{acquireOK: true}
	err := runLockedTick(context.Background(), lock, time.Hour, lg(), func(ctx context.Context) error {
		if ctx.Err() != nil {
			t.Fatalf("ctx must be live during run")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("happy path err: %v", err)
	}
	if !lock.released.Load() {
		t.Fatalf("lock must be released after tick")
	}
}

// TestRunLockedTick_NoRenewAfterRelease 续租 goroutine 必须在 Release 之前 join 退出：
// Release 后绝不再有 Renew 调用（否则迟到 Renew 误判 owner 丢失 + goroutine 泄漏）。
// 用短续租周期让续租活跃，orderLock 记录 Release 后是否仍发生 Renew。
func TestRunLockedTick_NoRenewAfterRelease(t *testing.T) {
	lock := &orderLock{}
	err := runLockedTick(context.Background(), lock, time.Millisecond, lg(), func(ctx context.Context) error {
		time.Sleep(20 * time.Millisecond) // 让续租至少跑几次
		return nil
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !lock.released.Load() {
		t.Fatalf("must release")
	}
	if lock.renewAfterRelease.Load() {
		t.Fatalf("Renew was called after Release — renew goroutine not joined before release")
	}
}

// orderLock 记录 Release 之后是否仍有 Renew 被调用（验证 join 正确性）。
type orderLock struct {
	released          atomic.Bool
	renewAfterRelease atomic.Bool
}

func (o *orderLock) Acquire(token string) (bool, error) { return true, nil }
func (o *orderLock) Renew(token string) (bool, error) {
	if o.released.Load() {
		o.renewAfterRelease.Store(true)
	}
	return true, nil
}
func (o *orderLock) Release(token string) error {
	o.released.Store(true)
	return nil
}

// TestRunLockedTick_RenewFailureAbortsInFlight 🔴 C3 核心：续租失败 → lockCtx 取消 →
// 在飞 run 立即收到取消并 abort（不继续推进）。验证「锁失效路径不双算」。
func TestRunLockedTick_RenewFailureAbortsInFlight(t *testing.T) {
	// renewAfter=0 + renewErr：第一次续租即失败 → 触发 cancel。
	lock := &fakeLock{acquireOK: true, renewErr: errors.New("renew failed")}

	aborted := make(chan struct{})
	var advancedAfterLoss atomic.Bool
	err := runLockedTick(context.Background(), lock, 10*time.Millisecond, lg(), func(ctx context.Context) error {
		// 模拟在飞批次：持续工作直到 lockCtx 被取消。
		select {
		case <-ctx.Done():
			close(aborted)
			return ctx.Err()
		case <-time.After(5 * time.Second):
			// 若 5s 内没被取消，说明续租失败没触发 abort → C3 失败。
			advancedAfterLoss.Store(true)
			return nil
		}
	})
	select {
	case <-aborted:
		// 正确：在飞批次被 abort。
	case <-time.After(2 * time.Second):
		t.Fatalf("in-flight run was not aborted after renew failure (C3 violated)")
	}
	if advancedAfterLoss.Load() {
		t.Fatalf("run continued after lock loss — double-count risk (C3 violated)")
	}
	if err == nil {
		t.Fatalf("aborted run should return ctx error")
	}
}

// TestRunLockedTick_RenewOwnerLostAborts 续租返回 false（owner 被抢）同样 abort 在飞批次。
func TestRunLockedTick_RenewOwnerLostAborts(t *testing.T) {
	lock := &fakeLock{acquireOK: true, renewFalse: true}
	aborted := make(chan struct{})
	_ = runLockedTick(context.Background(), lock, 10*time.Millisecond, lg(), func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			close(aborted)
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return nil
		}
	})
	select {
	case <-aborted:
	case <-time.After(2 * time.Second):
		t.Fatalf("owner-lost renew must abort in-flight run (C3)")
	}
}

// TestRunLockedTick_TwoReplicasOnlyOneRuns 两副本并发抢同一把锁，只有一个跑 run，另一个跳过。
// 消除调度敏感：持锁副本进入 run 后阻塞，直到两副本都已调用过 Acquire（acquireCalls==2），
// 确保「第二副本在第一副本仍持锁时竞争」，而非第一副本跑完释放后第二副本才抢（那会假阳性双跑）。
func TestRunLockedTick_TwoReplicasOnlyOneRuns(t *testing.T) {
	shared := &sharedLock{}
	var runCount atomic.Int32
	release := make(chan struct{})

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = runLockedTick(context.Background(), shared, time.Hour, lg(), func(ctx context.Context) error {
				runCount.Add(1)
				// 持锁直到两副本都已尝试 Acquire（含抢不到的那个），保证竞争发生在持锁期间。
				for shared.acquireCalls.Load() < 2 {
					time.Sleep(time.Millisecond)
				}
				<-release
				return nil
			})
		}()
	}
	// 两副本都 Acquire 过后放行。轮询等待，避免依赖 sleep 时长。
	for shared.acquireCalls.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()

	if runCount.Load() != 1 {
		t.Fatalf("exactly one replica must run the tick, got %d", runCount.Load())
	}
}

// sharedLock 模拟 Redis SET NX 单 owner 语义（多 goroutine 抢同一把锁）。
type sharedLock struct {
	mu           sync.Mutex
	owner        string
	acquireCalls atomic.Int32
}

func (s *sharedLock) Acquire(token string) (bool, error) {
	s.acquireCalls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owner != "" {
		return false, nil
	}
	s.owner = token
	return true, nil
}

func (s *sharedLock) Renew(token string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.owner == token, nil
}

func (s *sharedLock) Release(token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owner == token {
		s.owner = ""
	}
	return nil
}
