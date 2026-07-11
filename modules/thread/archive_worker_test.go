package thread

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubArchiveDB 用 stack of batch sizes 模拟"每次 ArchiveStaleBatch 影响多少行"。
type stubArchiveDB struct {
	batches []int64
	calls   int
	err     error
	// 记录每次调用收到的参数，用于断言。
	gotThresholds   []time.Time
	gotBatchSizes   []int
	gotVersions     []int64
	gotChannelTypes []uint8
}

func (s *stubArchiveDB) ArchiveStaleBatch(threshold time.Time, batchSize int, version int64, channelType uint8) (int64, error) {
	s.gotThresholds = append(s.gotThresholds, threshold)
	s.gotBatchSizes = append(s.gotBatchSizes, batchSize)
	s.gotVersions = append(s.gotVersions, version)
	s.gotChannelTypes = append(s.gotChannelTypes, channelType)
	if s.err != nil {
		return 0, s.err
	}
	if s.calls >= len(s.batches) {
		s.calls++
		return 0, nil
	}
	rows := s.batches[s.calls]
	s.calls++
	return rows, nil
}

type stubVersionGen struct {
	next int64
}

func (s *stubVersionGen) GenSeq(_ string) (int64, error) {
	return atomic.AddInt64(&s.next, 1), nil
}

type errVersionGen struct{ err error }

func (e *errVersionGen) GenSeq(_ string) (int64, error) { return 0, e.err }

func newTestWorker(cfg ArchiveConfig, db archiveDB, gen versionGen, now time.Time) *ArchiveWorker {
	return &ArchiveWorker{
		cfg: cfg,
		db:  db,
		gen: gen,
		now: func() time.Time { return now },
		Log: log.NewTLog("ThreadArchiveWorkerTest"),
	}
}

func defaultCfg() ArchiveConfig {
	return ArchiveConfig{
		Enabled:    true,
		Threshold:  3 * 24 * time.Hour,
		Interval:   time.Hour,
		BatchSize:  100,
		BatchSleep: 0,
	}
}

func TestRunOnce_DisabledByThreshold(t *testing.T) {
	cfg := defaultCfg()
	cfg.Threshold = 0
	db := &stubArchiveDB{}
	w := newTestWorker(cfg, db, &stubVersionGen{}, time.Now())

	rows, err := w.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(0), rows)
	assert.Equal(t, 0, db.calls, "should not call DB when threshold=0")
}

func TestRunOnce_DisabledByBatchSize(t *testing.T) {
	cfg := defaultCfg()
	cfg.BatchSize = 0
	db := &stubArchiveDB{}
	w := newTestWorker(cfg, db, &stubVersionGen{}, time.Now())

	rows, err := w.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(0), rows)
	assert.Equal(t, 0, db.calls)
}

func TestRunOnce_SingleBatchThenStop(t *testing.T) {
	cfg := defaultCfg()
	cfg.BatchSize = 100
	// 一次只归档了 30 条，少于 batchSize → 应该停下
	db := &stubArchiveDB{batches: []int64{30}}
	w := newTestWorker(cfg, db, &stubVersionGen{}, time.Now())

	rows, err := w.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(30), rows)
	assert.Equal(t, 1, db.calls)
}

func TestRunOnce_LoopsUntilUnderBatchSize(t *testing.T) {
	cfg := defaultCfg()
	cfg.BatchSize = 100
	// 满批 → 满批 → 50 (< batchSize)，应跑 3 次
	db := &stubArchiveDB{batches: []int64{100, 100, 50}}
	w := newTestWorker(cfg, db, &stubVersionGen{}, time.Now())

	rows, err := w.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(250), rows)
	assert.Equal(t, 3, db.calls)
}

func TestRunOnce_PassesCorrectThreshold(t *testing.T) {
	cfg := defaultCfg()
	cfg.Threshold = 5 * 24 * time.Hour
	fixedNow := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	db := &stubArchiveDB{batches: []int64{10}}
	w := newTestWorker(cfg, db, &stubVersionGen{}, fixedNow)

	_, err := w.RunOnce(context.Background())
	require.NoError(t, err)
	require.Len(t, db.gotThresholds, 1)
	expected := fixedNow.Add(-5 * 24 * time.Hour)
	assert.Equal(t, expected, db.gotThresholds[0])
	assert.Equal(t, 100, db.gotBatchSizes[0])
}

func TestRunOnce_VersionMonotonicallyIncrementsPerBatch(t *testing.T) {
	cfg := defaultCfg()
	db := &stubArchiveDB{batches: []int64{100, 100, 50}}
	w := newTestWorker(cfg, db, &stubVersionGen{}, time.Now())

	_, err := w.RunOnce(context.Background())
	require.NoError(t, err)
	require.Len(t, db.gotVersions, 3)
	// 每批用 GenSeq 拿一个新版本号
	assert.Equal(t, int64(1), db.gotVersions[0])
	assert.Equal(t, int64(2), db.gotVersions[1])
	assert.Equal(t, int64(3), db.gotVersions[2])
}

func TestRunOnce_GenSeqErrorAborts(t *testing.T) {
	cfg := defaultCfg()
	db := &stubArchiveDB{batches: []int64{100, 100}}
	wantErr := errors.New("seq down")
	w := newTestWorker(cfg, db, &errVersionGen{err: wantErr}, time.Now())

	rows, err := w.RunOnce(context.Background())
	assert.ErrorIs(t, err, wantErr)
	assert.Equal(t, int64(0), rows)
	assert.Equal(t, 0, db.calls)
}

func TestRunOnce_DBErrorAborts(t *testing.T) {
	cfg := defaultCfg()
	wantErr := errors.New("dial down")
	db := &stubArchiveDB{err: wantErr}
	w := newTestWorker(cfg, db, &stubVersionGen{}, time.Now())

	rows, err := w.RunOnce(context.Background())
	assert.ErrorIs(t, err, wantErr)
	assert.Equal(t, int64(0), rows)
}

func TestRunOnce_ContextCanceledBetweenBatches(t *testing.T) {
	cfg := defaultCfg()
	// 永远满批，否则不进 sleep 分支
	db := &stubArchiveDB{batches: []int64{100, 100, 100, 100, 100}}
	w := newTestWorker(cfg, db, &stubVersionGen{}, time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立刻取消，第一次循环顶端就会退出
	rows, err := w.RunOnce(ctx)
	assert.ErrorIs(t, err, context.Canceled)
	// 因为取消发生在第一轮 ctx.Err 检查处，rows 应该是 0
	assert.Equal(t, int64(0), rows)
}

func TestRunOnce_BatchSleepRespectsContextCancel(t *testing.T) {
	cfg := defaultCfg()
	cfg.BatchSleep = 1 * time.Second // 故意长一点
	db := &stubArchiveDB{batches: []int64{100, 100, 100}}
	w := newTestWorker(cfg, db, &stubVersionGen{}, time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	// 跑起来后 10ms 取消，sleep 必须立刻退出而不是等 1s
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := w.RunOnce(ctx)
	elapsed := time.Since(start)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Less(t, elapsed, 500*time.Millisecond, "sleep must abort on ctx cancel")
}

func TestStart_TicksAndStops(t *testing.T) {
	cfg := defaultCfg()
	cfg.Interval = 20 * time.Millisecond
	db := newSignalArchiveDB()
	w := newTestWorker(cfg, db, &stubVersionGen{}, time.Now())

	w.Start(context.Background())

	// channel-based handoff，避免 sleep-poll 在 CI 上抖动
	select {
	case <-db.firstCall:
	case <-time.After(2 * time.Second):
		w.Stop()
		t.Fatal("ticker did not trigger RunOnce within 2s")
	}

	w.Stop()
	snapshot := atomic.LoadInt64(&db.calls)
	assert.Greater(t, snapshot, int64(0))

	// Stop 后不应再有新调用
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, snapshot, atomic.LoadInt64(&db.calls), "no DB calls after Stop")
}

// signalArchiveDB 第一次被调时 close firstCall，便于测试 channel 等待。
type signalArchiveDB struct {
	calls     int64
	firstCall chan struct{}
	once      sync.Once
}

func newSignalArchiveDB() *signalArchiveDB {
	return &signalArchiveDB{firstCall: make(chan struct{})}
}

func (s *signalArchiveDB) ArchiveStaleBatch(time.Time, int, int64, uint8) (int64, error) {
	atomic.AddInt64(&s.calls, 1)
	s.once.Do(func() { close(s.firstCall) })
	return 0, nil // 立刻返回 0 行，RunOnce 第一批就退出
}

// TestStart_DisabledAfterEnabledStopsOldWorker 验证 reviewer 指出的热更新安全性：
// 先以 enabled=true 启动 worker，再调用 Start 时 enabled 已改为 false，旧 goroutine
// 必须被停掉而不是被孤儿。生产路径目前只启动一次，但这保证了未来热更新无泄漏。
func TestStart_DisabledAfterEnabledStopsOldWorker(t *testing.T) {
	cfg := defaultCfg()
	cfg.Interval = 20 * time.Millisecond
	db := newSignalArchiveDB()
	w := newTestWorker(cfg, db, &stubVersionGen{}, time.Now())

	w.Start(context.Background())
	select {
	case <-db.firstCall:
	case <-time.After(2 * time.Second):
		w.Stop()
		t.Fatal("first Start should have produced a tick")
	}
	snapshot := atomic.LoadInt64(&db.calls)

	// 模拟热更新把 enabled 翻成 false 再 Start
	w.cfg.Enabled = false
	w.Start(context.Background())

	// 旧 goroutine 必须停了：再等一段时间不应出现新 call
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, snapshot, atomic.LoadInt64(&db.calls),
		"old worker goroutine must be cancelled before disabled-Start returns")
}

func TestStart_DisabledIsNoop(t *testing.T) {
	cfg := defaultCfg()
	cfg.Enabled = false
	db := &stubArchiveDB{batches: []int64{100, 100}}
	w := newTestWorker(cfg, db, &stubVersionGen{}, time.Now())

	w.Start(context.Background())
	defer w.Stop()
	time.Sleep(30 * time.Millisecond)
	assert.Equal(t, 0, db.calls, "disabled worker must not tick")
}

// TestStart_ThresholdZeroStartsGoroutine 验证 reviewer 指出的启动门：Threshold=0 是 config
// 明文支持的模式（DM_THREAD_AUTO_ARCHIVE_DAYS=0 → 禁用时间归档但 Enabled 仍 true）。
// 启动门不再因 Threshold<=0 而拒起 goroutine。注意：本测试只断言 goroutine 确实启动
// （w.cancel 非 nil）；Threshold=0 时 RunOnce 会在碰 DB 前短路，故不断言归档行为。
func TestStart_ThresholdZeroStartsGoroutine(t *testing.T) {
	cfg := defaultCfg()
	cfg.Threshold = 0
	cfg.Interval = 20 * time.Millisecond
	db := newSignalArchiveDB()
	w := newTestWorker(cfg, db, &stubVersionGen{}, time.Now())

	w.Start(context.Background())
	defer w.Stop()

	// ticker 必须起：RunOnce 会被调到。但 signalArchiveDB 只在 ArchiveStaleBatch 被调时
	// close firstCall；而 Threshold=0 时 RunOnce 内部短路、不会调 ArchiveStaleBatch。
	// 故这里不等 firstCall，改为验证 goroutine 确实启动了（cancel 非 nil）。
	assert.NotNil(t, w.cancel, "ticker goroutine must start even when Threshold=0")
}
