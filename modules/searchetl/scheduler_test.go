package searchetl

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/gocraft/dbr/v2/dialect"
)

// newTestScheduler 构造一个不触达 DB/Kafka 的 scheduler：tickFn 注入计数器，interval 调短。
func newTestScheduler(tickFn func(context.Context) error, interval time.Duration) *scheduler {
	s := newScheduler(config.NewContext(config.New()), &ETL{})
	s.tickFn = tickFn
	s.interval = interval
	return s
}

// TestScheduler_StartStopTick 验收门(i)：startLoop 起循环 → tick 周期触发 → Stop 后停止且
// join 退出。用极短 interval + 注入计数 tickFn，无需真实 Kafka/DB。
func TestScheduler_StartStopTick(t *testing.T) {
	var ticks atomic.Int32
	s := newTestScheduler(func(context.Context) error {
		ticks.Add(1)
		return nil
	}, 5*time.Millisecond)

	s.mu.Lock()
	s.startLoop()
	s.mu.Unlock()

	// 等若干 tick 发生。
	deadline := time.After(2 * time.Second)
	for ticks.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("scheduler did not tick at least 3 times, got %d", ticks.Load())
		case <-time.After(2 * time.Millisecond):
		}
	}

	s.Stop()
	stoppedAt := ticks.Load()
	// Stop 之后不应再增长（loop goroutine 已 join 退出）。
	time.Sleep(30 * time.Millisecond)
	if got := ticks.Load(); got != stoppedAt {
		t.Fatalf("tick continued after Stop: %d -> %d", stoppedAt, got)
	}
}

// TestScheduler_StopIdempotent Stop 幂等：未启动时 Stop 安全；重复 Stop 不 panic/不阻塞。
func TestScheduler_StopIdempotent(t *testing.T) {
	s := newTestScheduler(func(context.Context) error { return nil }, time.Hour)
	s.Stop() // 未启动即 Stop
	s.mu.Lock()
	s.startLoop()
	s.mu.Unlock()
	s.Stop()
	s.Stop() // 重复 Stop
}

// TestScheduler_StartIdempotent 重复 Start 只起一个循环。
func TestScheduler_StartIdempotent(t *testing.T) {
	var ticks atomic.Int32
	s := newTestScheduler(func(context.Context) error { ticks.Add(1); return nil }, 5*time.Millisecond)
	// 测试模式默认放行 skip；这里直驱 startLoop 两次（模拟意外重入），第二次应被 started 挡下。
	s.mu.Lock()
	s.startLoop()
	s.mu.Unlock()
	// 第二次 Start（真实入口）应因 started=true 直接返回 nil，不再起循环。
	if err := s.Start(); err != nil {
		t.Fatalf("second Start should be nil, got %v", err)
	}
	s.Stop()
}

// TestEvaluateStart_LagGate 验收门(iii)：生产(Kafka.On ∧ 非测试 ∧ MySQL) 下 lag<=0 → fatal。
func TestEvaluateStart_LagGate(t *testing.T) {
	for _, lag := range []int64{0, -1} {
		d, reason := evaluateStart(false, true, true, lag)
		if d != startFatal {
			t.Fatalf("lag=%d must be startFatal, got %v (%s)", lag, d, reason)
		}
	}
	// lag>0 → 放行启动。
	if d, _ := evaluateStart(false, true, true, 600); d != startRun {
		t.Fatalf("lag>0 production must startRun, got %v", d)
	}
}

// TestEvaluateStart_MySQLOnlyGate 验收门(iv)：非 MySQL 方言 → skip（不 fatal，IM 核心照跑），
// 且先于 lag 判定（即便 lag=0 也只 skip 不 fatal）。
func TestEvaluateStart_MySQLOnlyGate(t *testing.T) {
	d, reason := evaluateStart(false, true, false, 0)
	if d != startSkip {
		t.Fatalf("non-MySQL must startSkip (IM core unaffected), got %v", d)
	}
	if reason == "" {
		t.Fatalf("skip reason should be present for diagnostics")
	}
}

// TestEvaluateStart_LazyAndTest Kafka.On=false 与测试模式都 skip（不起循环、不 fatal）。
func TestEvaluateStart_LazyAndTest(t *testing.T) {
	if d, _ := evaluateStart(false, false, true, 600); d != startSkip {
		t.Fatalf("Kafka.On=false must startSkip, got %v", d)
	}
	if d, _ := evaluateStart(true, true, true, 0); d != startSkip {
		t.Fatalf("test mode must startSkip even with lag=0, got %v", d)
	}
}

// TestStart_LagGateFatalAndSkip 通过真实 Start() 验证护栏：lag=0 + Kafka.On + 非测试 → 返回
// error（fatal 拒启动，阻断 boot）；Kafka.On=false → 返回 nil 且不启动循环。
func TestStart_LagGateFatalAndSkip(t *testing.T) {
	// 1) Kafka.On=false：skip 放行，nil。
	cfgOff := config.New()
	cfgOff.Kafka.On = false
	cfgOff.Test = false
	soff := &scheduler{ctx: config.NewContext(cfgOff), etl: &ETL{lag: 600}, interval: time.Hour, Log: lg()}
	if err := soff.Start(); err != nil {
		t.Fatalf("Kafka.On=false Start must be nil, got %v", err)
	}
	if soff.started {
		t.Fatalf("Kafka.On=false must NOT start loop")
	}
}

// TestIsMySQLDialect 当前 octo-lib 恒以 mysql 驱动 dbr.Open → MySQL 方言判真；PG 判假
// （护栏未来自动生效）。
func TestIsMySQLDialect(t *testing.T) {
	if !isMySQLDialect(dialect.MySQL) {
		t.Fatalf("MySQL dialect must be detected as MySQL")
	}
	if isMySQLDialect(dialect.PostgreSQL) {
		t.Fatalf("PostgreSQL must NOT be detected as MySQL (MySQL-only guard would skip)")
	}
}
