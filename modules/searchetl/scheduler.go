package searchetl

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
	"go.uber.org/zap"
)

// scheduler 驱动 searchetl 慢游标增量抽取的定时 tick 循环（YUJ-5012 票4a，安全 checkpoint）。
//
// 这是让 searchetl 这套「从未在生产真跑过」的代码第一次真跑的生命周期挂点：把 opanalytics
// 的每日 cron 范式改成分钟级 time.Ticker（tickInterval()），每个 tick 调一次 ETL.RunIncremental。
//
// 🔴 本期范围（票4a，严格）：**仅慢游标一个 tick**（lag 维持 600/现状保守值）。不挂快游标
// （票4b），不改 minTickSeconds clamp。跨副本互斥/失锁 abort 由 RunIncremental 内的 Redis
// run-lock（C3）负责，scheduler 只管「按节奏触发 + 生命周期」。
//
// 启动护栏（plan v2 §4/§5 + ReviewBot 建议4 + codex P1，把人盯变代码挡）：
//   - Kafka.On=false：不启动 tick 循环（惰性零开销，与 RunIncremental 惰性短路一致）。
//   - 🔴 慢游标 lag>0 断言：生产环境 lag<=0 → fatal 拒启动（lag=0 = 静默漏读，STOP#2）。
//   - 🔴 MySQL-only 护栏：非 MySQL 方言（PG 等）→ 跳过 searchetl scheduler（不 fatal，
//     IM 核心照跑），并明确日志「本期 MySQL-only，PG 见 follow-up」。
//   - 测试模式（cfg.Test）：不自动起循环（lag=0 仅在此放行；生命周期单测走 startLoop 直驱）。
type scheduler struct {
	log.Log
	ctx      *config.Context
	etl      *ETL
	interval time.Duration
	// tickFn 是每个 tick 执行的动作（默认 etl.RunIncremental）。抽成字段便于生命周期单测
	// 注入计数器，无需真实 Kafka/DB 即可验证 Start/Stop/tick 触发（验收门 i）。
	tickFn func(context.Context) error

	mu      sync.Mutex
	started bool
	cancel  context.CancelFunc
	done    chan struct{}
}

// newScheduler 创建慢游标 scheduler。
func newScheduler(ctx *config.Context, etl *ETL) *scheduler {
	return &scheduler{
		Log:      log.NewTLog("SearchETLScheduler"),
		ctx:      ctx,
		etl:      etl,
		interval: tickInterval(),
		tickFn:   etl.RunIncremental,
	}
}

// startDecision 编码 Start 的护栏裁决（便于纯函数单测，与 DB/server 解耦）。
type startDecision int

const (
	startRun   startDecision = iota // 通过护栏，启动 tick 循环
	startSkip                       // 不启动，但放行 boot（返回 nil；IM 核心照跑）
	startFatal                      // 拒启动（返回 error，阻断 boot）
)

// evaluateStart 是启动护栏的纯逻辑（验收门 iii/iv + 惰性/测试跳过路径，均可无 DB 单测）。
//
// 优先级：测试模式 > Kafka 惰性 > MySQL-only > lag>0。MySQL-only 先于 lag 判定——非 MySQL
// 时根本不该起 searchetl，连 lag 都不评估，直接 skip（IM 核心照跑）。
func evaluateStart(testMode, kafkaOn, mysqlDialect bool, lag int64) (startDecision, string) {
	if testMode {
		return startSkip, "test mode: scheduler not auto-started (lag=0 allowed only here)"
	}
	if !kafkaOn {
		return startSkip, "Kafka.On=false: scheduler not started (lazy no-op, zero overhead)"
	}
	if !mysqlDialect {
		return startSkip, "MySQL-only this phase: non-MySQL dialect detected, searchetl scheduler skipped (PG is a follow-up); IM core unaffected"
	}
	if lag <= 0 {
		return startFatal, fmt.Sprintf("slow-cursor lag must be > 0 in production (got %d); lag=0 risks silent missed reads (STOP#2 enforced as code)", lag)
	}
	return startRun, "ok"
}

// isMySQLDialect 判定 dbr 会话方言是否为 MySQL。octo-lib 当前恒以 mysql 驱动 dbr.Open，故
// 此刻恒真；一旦未来引入 PG 方言（改驱动）即转假，触发 MySQL-only 护栏自动拒启 scheduler。
func isMySQLDialect(d dbr.Dialect) bool {
	return d == dialect.MySQL
}

// Start 启动慢游标 scheduler（幂等）。返回 error 即 fatal 拒启动（由 octo-lib module.Start
// 传播阻断 boot）；返回 nil 表示已启动或按护栏跳过（boot 继续）。
func (s *scheduler) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return nil
	}

	cfg := s.ctx.GetConfig()
	// 仅当「非测试 ∧ Kafka.On」即真要起循环时才探测方言（避免在测试/惰性路径触达 DB 层）。
	mysqlDialect := true
	if !cfg.Test && cfg.Kafka.On {
		mysqlDialect = isMySQLDialect(s.ctx.DB().Dialect)
	}

	switch decision, reason := evaluateStart(cfg.Test, cfg.Kafka.On, mysqlDialect, s.etl.lag); decision {
	case startFatal:
		// 🔴 fatal：返回 error 让 module.Start 阻断整个 boot（fail-closed）。
		return fmt.Errorf("searchetl scheduler refuses to start: %s", reason)
	case startSkip:
		s.Info("searchetl scheduler not started", zap.String("reason", reason))
		return nil
	}

	s.startLoop()
	s.Info("searchetl slow-cursor scheduler started",
		zap.Duration("tick", s.interval), zap.Int64("lag_seconds", s.etl.lag))
	return nil
}

// startLoop 实际拉起后台 tick goroutine（护栏通过后调用）。抽出来供生命周期单测直驱，
// 绕过仅生产相关的护栏（验收门 i：Start/Stop/tick 触发）。调用方须持 s.mu。
func (s *scheduler) startLoop() {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done = make(chan struct{})
	s.started = true
	go s.loop(ctx)
}

// loop 是慢游标 tick 主循环：每 interval 触发一次 tick，直到 ctx 取消（Stop）。
func (s *scheduler) loop(ctx context.Context) {
	defer close(s.done)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.Info("searchetl scheduler loop stopped")
			return
		case <-t.C:
			s.tick(ctx)
		}
	}
}

// tick 触发一轮慢游标增量。错误只记日志不中断循环（下个 tick 重试；跨副本/失锁安全由
// RunIncremental 内的 run-lock + CAS fence 保证）。
func (s *scheduler) tick(ctx context.Context) {
	if err := s.tickFn(ctx); err != nil {
		s.Error("searchetl scheduled incremental failed", zap.Error(err))
	}
}

// stopJoinTimeout 上限 Stop 等待 tick 循环退出的时长。超时则不再死等 join——一个 tick 可能
// 正卡在**非 context 感知**的 DB 调用里（etl_db 的 dbr Begin/Load/Exec 不吃 ctx），无界 join
// 会让进程关停被拖死。超时即放弃 join（仅泄漏一个终将随进程退出的 goroutine）+ 大声告警，
// 保证关停不被卡住的 MySQL 调用扣作人质。
const stopJoinTimeout = 30 * time.Second

// Stop 停止 scheduler（幂等）：取消循环 ctx 并 join goroutine 退出。join 有上限
// （stopJoinTimeout），避免被卡在非 ctx 感知 DB 调用里的 tick 拖死进程关停。
func (s *scheduler) Stop() {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return
	}
	s.started = false
	cancel, done := s.cancel, s.done
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done: // 循环 goroutine 已干净退出
			s.Info("searchetl scheduler stopped")
		case <-time.After(stopJoinTimeout):
			// tick 卡在非 ctx 感知的 DB 调用：放弃 join 不阻塞关停（goroutine 随进程退出）。
			s.Warn("searchetl scheduler stop: tick did not exit within timeout, abandoning join",
				zap.Duration("timeout", stopJoinTimeout))
		}
		return
	}
	s.Info("searchetl scheduler stopped")
}
