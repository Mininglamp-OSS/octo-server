package group

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubReconcileStore 模拟持久层：可配置待返回的孤儿群、成员列表与注入错误，
// 并记录 SetChannelSynced 调用，供断言修复动作。
type stubReconcileStore struct {
	orphans      []string
	queryErr     error
	subscribers  map[string][]string
	subErr       error
	setErr       error
	gotCutoff    time.Time
	gotLimit     int
	syncedGroups []string // 记录 SetChannelSynced(x, 1) 的入参顺序
	queryCalls   int
}

func (s *stubReconcileStore) QueryUnsyncedChannelGroups(cutoff time.Time, limit int) ([]string, error) {
	s.queryCalls++
	s.gotCutoff = cutoff
	s.gotLimit = limit
	if s.queryErr != nil {
		return nil, s.queryErr
	}
	return s.orphans, nil
}

func (s *stubReconcileStore) SubscribableMemberUIDs(groupNo string) ([]string, error) {
	if s.subErr != nil {
		return nil, s.subErr
	}
	return s.subscribers[groupNo], nil
}

func (s *stubReconcileStore) SetChannelSynced(groupNo string, synced int) error {
	if s.setErr != nil {
		return s.setErr
	}
	if synced == 1 {
		s.syncedGroups = append(s.syncedGroups, groupNo)
	}
	return nil
}

// stubChannelCreator 记录补建频道调用；failFor 中的 group 返回错误，模拟 IM 侧失败。
type stubChannelCreator struct {
	created []*config.ChannelCreateReq
	failFor map[string]bool
	err     error
}

func (c *stubChannelCreator) IMCreateOrUpdateChannel(req *config.ChannelCreateReq) error {
	c.created = append(c.created, req)
	if c.err != nil {
		return c.err
	}
	if c.failFor[req.ChannelID] {
		return errors.New("im create failed for " + req.ChannelID)
	}
	return nil
}

func newTestReconcileWorker(cfg ReconcileConfig, store reconcileStore, im channelCreator, now time.Time) *ReconcileWorker {
	return &ReconcileWorker{
		cfg:   cfg,
		store: store,
		im:    im,
		now:   func() time.Time { return now },
		Log:   log.NewTLog("GroupChannelReconcileWorkerTest"),
	}
}

func testReconcileCfg() ReconcileConfig {
	return ReconcileConfig{
		Enabled:   true,
		Interval:  time.Minute,
		Grace:     2 * time.Minute,
		BatchSize: 100,
	}
}

// Test_Issue394_ReconcileRecreatesOrphanChannel 是核心回归：一个 channel_synced=0 的
// 孤儿群（CreateGroup 补偿删除失败 / 崩溃窗口的残留），reconcile worker 必须幂等补建其
// WuKongIM 频道（带当前订阅成员），并翻 channel_synced=1，从而消除「可查询但无 backing
// 频道」的永久孤儿。worker 出现之前该行为不存在（孤儿永久残留）→ 本测试是 RED→GREEN 的信号。
func Test_Issue394_ReconcileRecreatesOrphanChannel(t *testing.T) {
	store := &stubReconcileStore{
		orphans:     []string{"g-orphan-1"},
		subscribers: map[string][]string{"g-orphan-1": {"u1", "u2"}},
	}
	im := &stubChannelCreator{}
	w := newTestReconcileWorker(testReconcileCfg(), store, im, time.Now())

	resolved, err := w.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, resolved, "the single orphan should be reconciled")

	require.Len(t, im.created, 1, "IM channel should be (re)created exactly once")
	got := im.created[0]
	assert.Equal(t, "g-orphan-1", got.ChannelID)
	assert.Equal(t, common.ChannelTypeGroup.Uint8(), got.ChannelType)
	assert.Equal(t, []string{"u1", "u2"}, got.Subscribers, "current members must be passed as subscribers")

	assert.Equal(t, []string{"g-orphan-1"}, store.syncedGroups, "orphan must be flipped channel_synced=1 after confirmed create")
}

// Test_Issue394_ReconcileGraceCutoff 断言扫描 cutoff = now - grace，且 limit = batch size，
// 保证 worker 不会和正在进行中的建群事务（尚未走到 channel_synced=1）竞争。
func Test_Issue394_ReconcileGraceCutoff(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	cfg := testReconcileCfg()
	cfg.Grace = 90 * time.Second
	cfg.BatchSize = 37
	store := &stubReconcileStore{}
	w := newTestReconcileWorker(cfg, store, &stubChannelCreator{}, now)

	_, err := w.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, now.Add(-90*time.Second), store.gotCutoff)
	assert.Equal(t, 37, store.gotLimit)
}

// Test_Issue394_ReconcileIMFailureRetries 断言 IM 补建失败时不翻 channel_synced，
// 该孤儿留待下个 tick 重试（最终一致，不会误标记为已修复）。
func Test_Issue394_ReconcileIMFailureRetries(t *testing.T) {
	store := &stubReconcileStore{
		orphans:     []string{"g-fail", "g-ok"},
		subscribers: map[string][]string{"g-fail": {"u1"}, "g-ok": {"u2"}},
	}
	im := &stubChannelCreator{failFor: map[string]bool{"g-fail": true}}
	w := newTestReconcileWorker(testReconcileCfg(), store, im, time.Now())

	resolved, err := w.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, resolved, "only the healthy orphan resolves")
	assert.Equal(t, []string{"g-ok"}, store.syncedGroups, "failed orphan must NOT be marked synced")
	assert.Len(t, im.created, 2, "both orphans attempted; failure does not abort the batch")
}

// Test_Issue394_ReconcileMarkFailureNotResolved 断言频道补建成功但翻标记失败时不计为
// 已修复——下个 tick 会再次命中并重试（幂等），避免「频道在但 DB 标记漂移」被误判修复。
func Test_Issue394_ReconcileMarkFailureNotResolved(t *testing.T) {
	store := &stubReconcileStore{
		orphans:     []string{"g1"},
		subscribers: map[string][]string{"g1": {"u1"}},
		setErr:      errors.New("db down"),
	}
	im := &stubChannelCreator{}
	w := newTestReconcileWorker(testReconcileCfg(), store, im, time.Now())

	resolved, err := w.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, resolved)
	assert.Len(t, im.created, 1, "channel still (idempotently) created")
}

// Test_Issue394_ReconcileSubscriberQueryFailureStillCreates 断言拉成员失败时仍补建频道
// （空订阅），至少消除「无 backing 频道」的硬缺陷；成员由后续订阅重载自愈。
func Test_Issue394_ReconcileSubscriberQueryFailureStillCreates(t *testing.T) {
	store := &stubReconcileStore{
		orphans: []string{"g1"},
		subErr:  errors.New("member query failed"),
	}
	im := &stubChannelCreator{}
	w := newTestReconcileWorker(testReconcileCfg(), store, im, time.Now())

	resolved, err := w.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, resolved)
	require.Len(t, im.created, 1)
	assert.Empty(t, im.created[0].Subscribers)
	assert.Equal(t, []string{"g1"}, store.syncedGroups)
}

func Test_Issue394_ReconcileDisabledByBatchSize(t *testing.T) {
	cfg := testReconcileCfg()
	cfg.BatchSize = 0
	store := &stubReconcileStore{orphans: []string{"g1"}}
	w := newTestReconcileWorker(cfg, store, &stubChannelCreator{}, time.Now())

	resolved, err := w.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, resolved)
	assert.Equal(t, 0, store.queryCalls, "must not query DB when batch size disabled")
}

func Test_Issue394_ReconcileQueryErrorPropagates(t *testing.T) {
	store := &stubReconcileStore{queryErr: errors.New("select failed")}
	w := newTestReconcileWorker(testReconcileCfg(), store, &stubChannelCreator{}, time.Now())

	resolved, err := w.RunOnce(context.Background())
	require.Error(t, err)
	assert.Equal(t, 0, resolved)
}

func Test_Issue394_ReconcileContextCancelStopsBatch(t *testing.T) {
	store := &stubReconcileStore{
		orphans:     []string{"g1", "g2", "g3"},
		subscribers: map[string][]string{},
	}
	im := &stubChannelCreator{}
	w := newTestReconcileWorker(testReconcileCfg(), store, im, time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消：RunOnce 拉到批后应在处理首个孤儿前就退出。
	resolved, err := w.RunOnce(ctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 0, resolved)
	assert.Empty(t, im.created, "cancelled context must not dispatch any IM create")
}

// Test_Issue394_ReconcileStartDisabledNoGoroutine 断言 Enabled=false 时 Start 不起 ticker。
func Test_Issue394_ReconcileStartDisabledNoGoroutine(t *testing.T) {
	cfg := testReconcileCfg()
	cfg.Enabled = false
	store := &stubReconcileStore{orphans: []string{"g1"}}
	w := newTestReconcileWorker(cfg, store, &stubChannelCreator{}, time.Now())

	w.Start(context.Background())
	defer w.Stop()
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, 0, store.queryCalls, "disabled worker must not tick")
}
