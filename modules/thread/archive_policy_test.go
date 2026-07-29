package thread

// =============================================================================
// 归档策略每 tick 重读（task inactive-hiding-user-control / P1）
//
// 配置迁入 system_settings 后，开关与阈值必须在**每轮 tick**重新解析，否则管理台
// 改值仍要重启进程才生效 —— 那正是本次迁移要消除的。这里用注入的 policy 直接验证
// 解析时机，不依赖 DB。
// =============================================================================

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// policyStub 返回可在运行中翻转的策略，并记录被解析的次数。
type policyStub struct {
	enabled   atomic.Bool
	threshold atomic.Int64 // time.Duration
	calls     atomic.Int64
}

func (p *policyStub) resolve() (bool, time.Duration) {
	p.calls.Add(1)
	return p.enabled.Load(), time.Duration(p.threshold.Load())
}

func newWorkerWithPolicy(cfg ArchiveConfig, db archiveDB, p *policyStub) *ArchiveWorker {
	w := newTestWorker(cfg, db, &stubVersionGen{}, time.Now())
	w.policy = p.resolve
	return w
}

// 注入 policy 后，cfg.Enabled / cfg.Threshold 必须被忽略 —— 生产路径靠这一点让
// system_settings 成为单一真源。
func TestRunOnce_PolicyOverridesCfg(t *testing.T) {
	cfg := defaultCfg()
	cfg.Enabled = false // cfg 说关
	cfg.Threshold = 0   // cfg 说没有阈值
	db := &stubArchiveDB{batches: []int64{1}}

	p := &policyStub{}
	p.enabled.Store(true) // policy 说开
	p.threshold.Store(int64(3 * 24 * time.Hour))
	w := newWorkerWithPolicy(cfg, db, p)

	rows, err := w.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(1), rows, "应以 policy 为准执行归档")
	assert.Equal(t, 1, db.calls)
}

func TestRunOnce_PolicyDisabledShortCircuits(t *testing.T) {
	db := &stubArchiveDB{batches: []int64{100}}
	p := &policyStub{}
	p.enabled.Store(false)
	p.threshold.Store(int64(3 * 24 * time.Hour))
	w := newWorkerWithPolicy(defaultCfg(), db, p)

	rows, err := w.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(0), rows)
	assert.Equal(t, 0, db.calls, "关闭时不得触碰 DB")
}

// threshold=0 保留 env 语义：禁用时间阈值但 worker 仍在跑。
func TestRunOnce_PolicyZeroThresholdShortCircuits(t *testing.T) {
	db := &stubArchiveDB{batches: []int64{100}}
	p := &policyStub{}
	p.enabled.Store(true)
	p.threshold.Store(0)
	w := newWorkerWithPolicy(defaultCfg(), db, p)

	rows, err := w.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(0), rows)
	assert.Equal(t, 0, db.calls)
}

// 核心用例：运行中把开关从 false 翻到 true，无需重启，下一 tick 即开始归档。
func TestStart_PolicyFlipTakesEffectWithoutRestart(t *testing.T) {
	cfg := defaultCfg()
	cfg.Interval = 20 * time.Millisecond
	db := newSignalArchiveDB()

	p := &policyStub{}
	p.enabled.Store(false) // 起步关闭
	p.threshold.Store(int64(3 * 24 * time.Hour))
	w := newWorkerWithPolicy(cfg, db, p)

	w.Start(context.Background())
	defer w.Stop()

	// 关闭态：ticker 照常起（保证可热开启），但不得归档。
	time.Sleep(80 * time.Millisecond)
	require.Equal(t, int64(0), atomic.LoadInt64(&db.calls), "关闭期间不得归档")
	require.Greater(t, p.calls.Load(), int64(0), "策略必须被逐 tick 解析，而非启动期冻结")

	// 热开启：无需 Stop/Start。
	p.enabled.Store(true)
	select {
	case <-db.firstCall:
	case <-time.After(2 * time.Second):
		t.Fatal("策略翻转后 2s 内未触发归档——配置未做到每 tick 重读")
	}
	assert.Greater(t, atomic.LoadInt64(&db.calls), int64(0))
}

// 反向：运行中关闭必须同样即时生效。
func TestStart_PolicyDisableTakesEffectWithoutRestart(t *testing.T) {
	cfg := defaultCfg()
	cfg.Interval = 20 * time.Millisecond
	db := newSignalArchiveDB()

	p := &policyStub{}
	p.enabled.Store(true)
	p.threshold.Store(int64(3 * 24 * time.Hour))
	w := newWorkerWithPolicy(cfg, db, p)

	w.Start(context.Background())
	defer w.Stop()

	select {
	case <-db.firstCall:
	case <-time.After(2 * time.Second):
		t.Fatal("开启态 2s 内未触发归档")
	}

	p.enabled.Store(false)
	// 让当前可能在途的一轮跑完，再取基线。
	time.Sleep(60 * time.Millisecond)
	snapshot := atomic.LoadInt64(&db.calls)
	time.Sleep(120 * time.Millisecond)
	assert.Equal(t, snapshot, atomic.LoadInt64(&db.calls),
		"关闭后不应再有新的归档批次")
}

// 未注入 policy 时回落 cfg —— 既有单测依赖这条路径，不得回归。
func TestResolvePolicy_FallsBackToCfgWhenNoResolver(t *testing.T) {
	cfg := defaultCfg()
	cfg.Enabled = true
	cfg.Threshold = 5 * 24 * time.Hour
	w := newTestWorker(cfg, &stubArchiveDB{}, &stubVersionGen{}, time.Now())

	enabled, threshold := w.resolvePolicy()
	assert.True(t, enabled)
	assert.Equal(t, 5*24*time.Hour, threshold)
}
