package group

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubReconcileDB 模拟对账 worker 的 DB 面：一批未同步群 + 每群成员映射，
// 并记录 MarkChannelSynced / DeleteOrphanGroup 的调用，用于断言收口动作。
type stubReconcileDB struct {
	unsynced    []string
	members     map[string][]string
	queryErr    error
	memberErr   map[string]error
	syncErr     map[string]error
	deleteErr   map[string]error
	syncedCalls []string
	deleteCalls []string
	gotBefore   time.Time
	gotLimit    int
}

func (s *stubReconcileDB) QueryUnsyncedGroupNos(before time.Time, limit int) ([]string, error) {
	s.gotBefore = before
	s.gotLimit = limit
	if s.queryErr != nil {
		return nil, s.queryErr
	}
	return s.unsynced, nil
}

func (s *stubReconcileDB) QueryMemberUIDsWithGroupNo(groupNo string) ([]string, error) {
	if s.memberErr != nil {
		if err := s.memberErr[groupNo]; err != nil {
			return nil, err
		}
	}
	return s.members[groupNo], nil
}

func (s *stubReconcileDB) MarkChannelSynced(groupNo string) error {
	if s.syncErr != nil {
		if err := s.syncErr[groupNo]; err != nil {
			return err
		}
	}
	s.syncedCalls = append(s.syncedCalls, groupNo)
	return nil
}

func (s *stubReconcileDB) DeleteOrphanGroup(groupNo string) error {
	if s.deleteErr != nil {
		if err := s.deleteErr[groupNo]; err != nil {
			return err
		}
	}
	s.deleteCalls = append(s.deleteCalls, groupNo)
	return nil
}

// stubReconcileIM 记录每次 IMCreateOrUpdateChannel 的请求，并可按 channelID 注入失败。
type stubReconcileIM struct {
	calls  []*config.ChannelCreateReq
	failed map[string]error
}

func (s *stubReconcileIM) IMCreateOrUpdateChannel(req *config.ChannelCreateReq) error {
	s.calls = append(s.calls, req)
	if s.failed != nil {
		if err := s.failed[req.ChannelID]; err != nil {
			return err
		}
	}
	return nil
}

func newTestReconciler(cfg ChannelReconcileConfig, db reconcileDB, im reconcileIM, now time.Time) *ChannelReconciler {
	return &ChannelReconciler{
		cfg: cfg,
		db:  db,
		im:  im,
		now: func() time.Time { return now },
		Log: log.NewTLog("GroupChannelReconcilerTest"),
	}
}

func reconcileTestCfg() ChannelReconcileConfig {
	return ChannelReconcileConfig{
		Enabled:   true,
		Interval:  time.Minute,
		Grace:     5 * time.Minute,
		BatchSize: 100,
	}
}

// Test_Issue394_ReconcileRecreatesChannelForOrphanWithMembers 复现并验证核心修复：
// CreateGroup「commit 后建频道」失败 + 补偿删除也失败留下的孤儿群（channel_synced=0，
// 成员仍在），对账 worker 必须幂等重建 IM 频道并标记 synced。
//
// 修复前：无对账路径，孤儿群永久存在——群可被列出却无 IM 频道，客户端永远无法发消息。
func Test_Issue394_ReconcileRecreatesChannelForOrphanWithMembers(t *testing.T) {
	db := &stubReconcileDB{
		unsynced: []string{"g1"},
		members:  map[string][]string{"g1": {"u1", "u2"}},
	}
	im := &stubReconcileIM{}
	w := newTestReconciler(reconcileTestCfg(), db, im, time.Unix(1_700_000_000, 0))

	recreated, deleted, err := w.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, recreated, "orphan with members must have its channel recreated")
	assert.Equal(t, 0, deleted)

	require.Len(t, im.calls, 1, "must call IMCreateOrUpdateChannel exactly once")
	assert.Equal(t, "g1", im.calls[0].ChannelID)
	assert.ElementsMatch(t, []string{"u1", "u2"}, im.calls[0].Subscribers, "subscribers must be the group's members")
	assert.Equal(t, []string{"g1"}, db.syncedCalls, "must mark synced after successful recreate")
	assert.Empty(t, db.deleteCalls, "a group with members must never be hard-deleted")
}

// Test_Issue394_ReconcileHardDeletesMemberlessOrphan 验证补偿删除部分成功的场景：
// group_member 已删、group 行残留 → 群无有效成员、不可恢复 → 硬删除孤儿群行。
func Test_Issue394_ReconcileHardDeletesMemberlessOrphan(t *testing.T) {
	db := &stubReconcileDB{
		unsynced: []string{"g_empty"},
		members:  map[string][]string{"g_empty": {}},
	}
	im := &stubReconcileIM{}
	w := newTestReconciler(reconcileTestCfg(), db, im, time.Unix(1_700_000_000, 0))

	recreated, deleted, err := w.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, recreated)
	assert.Equal(t, 1, deleted, "memberless orphan must be hard-deleted")

	assert.Empty(t, im.calls, "must not recreate a channel for a memberless orphan")
	assert.Equal(t, []string{"g_empty"}, db.deleteCalls)
	assert.Empty(t, db.syncedCalls)
}

// Test_Issue394_ReconcileLeavesUnsyncedWhenIMStillFailing 验证幂等重试语义：
// 若重建 IM 频道仍失败，worker 不标记 synced、不删除——群留待下一轮重试，绝不丢数据。
func Test_Issue394_ReconcileLeavesUnsyncedWhenIMStillFailing(t *testing.T) {
	db := &stubReconcileDB{
		unsynced: []string{"g_imdown"},
		members:  map[string][]string{"g_imdown": {"u1"}},
	}
	im := &stubReconcileIM{failed: map[string]error{"g_imdown": errors.New("IM still down")}}
	w := newTestReconciler(reconcileTestCfg(), db, im, time.Unix(1_700_000_000, 0))

	recreated, deleted, err := w.RunOnce(context.Background())
	require.NoError(t, err, "a single group's IM failure must not fail the whole batch")
	assert.Equal(t, 0, recreated)
	assert.Equal(t, 0, deleted)
	assert.Empty(t, db.syncedCalls, "must not mark synced when the channel recreate failed")
	assert.Empty(t, db.deleteCalls, "must not delete a group that still has members")
}

// Test_Issue394_ReconcileProcessesMixedBatchAndContinuesOnPerItemError 验证一个群
// 出错不影响同批其它群，且混合场景（重建 + 删除）都被正确收口。
func Test_Issue394_ReconcileProcessesMixedBatchAndContinuesOnPerItemError(t *testing.T) {
	db := &stubReconcileDB{
		unsynced: []string{"g_members", "g_qerr", "g_empty"},
		members: map[string][]string{
			"g_members": {"u1"},
			"g_empty":   {},
		},
		memberErr: map[string]error{"g_qerr": errors.New("transient query error")},
	}
	im := &stubReconcileIM{}
	w := newTestReconciler(reconcileTestCfg(), db, im, time.Unix(1_700_000_000, 0))

	recreated, deleted, err := w.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, recreated, "g_members must be recreated despite g_qerr failing")
	assert.Equal(t, 1, deleted, "g_empty must be deleted despite g_qerr failing")
	assert.Equal(t, []string{"g_members"}, db.syncedCalls)
	assert.Equal(t, []string{"g_empty"}, db.deleteCalls)
}

// Test_Issue394_ReconcileGracePeriodPassedToQuery 验证宽限期：查询下界必须是 now-Grace，
// 使得刚建、IM 创建可能仍在途的新群被跳过，不与建群主路径抢跑。
func Test_Issue394_ReconcileGracePeriodPassedToQuery(t *testing.T) {
	db := &stubReconcileDB{}
	now := time.Unix(1_700_000_000, 0)
	cfg := reconcileTestCfg()
	cfg.Grace = 10 * time.Minute
	w := newTestReconciler(cfg, db, &stubReconcileIM{}, now)

	_, _, err := w.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, now.Add(-10*time.Minute), db.gotBefore, "query cutoff must be now-Grace")
	assert.Equal(t, cfg.BatchSize, db.gotLimit)
}

// Test_Issue394_ReconcileQueryErrorSurfaces 验证批量查询失败时向上返回错误（供 Start 记日志）。
func Test_Issue394_ReconcileQueryErrorSurfaces(t *testing.T) {
	db := &stubReconcileDB{queryErr: errors.New("db down")}
	w := newTestReconciler(reconcileTestCfg(), db, &stubReconcileIM{}, time.Unix(1_700_000_000, 0))

	_, _, err := w.RunOnce(context.Background())
	require.Error(t, err)
}

// Test_Issue394_ReconcileDisabledConfigNoop 验证 BatchSize<=0 时 RunOnce 直接空转，不查库。
func Test_Issue394_ReconcileDisabledConfigNoop(t *testing.T) {
	db := &stubReconcileDB{unsynced: []string{"g1"}, members: map[string][]string{"g1": {"u1"}}}
	cfg := reconcileTestCfg()
	cfg.BatchSize = 0
	w := newTestReconciler(cfg, db, &stubReconcileIM{}, time.Unix(1_700_000_000, 0))

	recreated, deleted, err := w.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Zero(t, recreated)
	assert.Zero(t, deleted)
	assert.True(t, db.gotBefore.IsZero(), "must not query when disabled")
}

// Test_Issue394_LoadChannelReconcileConfigDefaults 验证默认开启 + 默认参数，且 env 显式关闭生效。
func Test_Issue394_LoadChannelReconcileConfigDefaults(t *testing.T) {
	t.Setenv(envReconcileEnabled, "")
	t.Setenv(envReconcileInterval, "")
	t.Setenv(envReconcileGrace, "")
	t.Setenv(envReconcileBatch, "")
	cfg := LoadChannelReconcileConfig()
	assert.True(t, cfg.Enabled, "reconciler must default ON so the #394 fix works out of the box")
	assert.Equal(t, defaultReconcileInterval, cfg.Interval)
	assert.Equal(t, defaultReconcileGrace, cfg.Grace)
	assert.Equal(t, defaultReconcileBatch, cfg.BatchSize)

	t.Setenv(envReconcileEnabled, "false")
	assert.False(t, LoadChannelReconcileConfig().Enabled, "explicit false must disable")

	t.Setenv(envReconcileEnabled, "true")
	t.Setenv(envReconcileBatch, "99999")
	assert.Equal(t, maxReconcileBatch, LoadChannelReconcileConfig().BatchSize, "oversized batch clamps to max")
}
