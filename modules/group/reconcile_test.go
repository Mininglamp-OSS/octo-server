package group

import (
	"errors"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/require"
)

// Issue #394 — CreateGroup 的补偿删除是非原子的：IM 频道创建失败后，若「已提交的 group/
// group_member 补偿删除」自身也失败（DB 抖动），或进程崩在 tx.Commit() 与 IM 创建之间，就会
// 留下「有 group 行、无 IM 频道」的孤儿群，客户端永远无法进入。
//
// 修复：建群事务内写 group_channel_outbox 行（原子提交），IM 频道确认后删除；后台 reconcile
// 定时扫描超过宽限期仍存在的行，用当前成员幂等重建频道 —— 或清理 group 行已消失的陈旧 outbox 行。
// 下列用例用注入桩（无需 live WuKongIM）覆盖验收要求的两种失败模式及其最终一致收口。

// seedOrphanGroup 直接落一个「已提交 + 待确认频道」的群：group 行、一名正常成员、以及一条
// outbox 行 —— 即 IM 创建失败(补偿删除也失败) 或 commit 后崩溃所留下的孤儿状态。
func seedOrphanGroup(t *testing.T, ctx *config.Context, groupNo string, members ...string) {
	t.Helper()
	db := NewDB(ctx)
	require.NoError(t, db.Insert(&Model{
		GroupNo:        groupNo,
		Name:           "orphan-" + groupNo,
		Status:         GroupStatusNormal,
		AllowExternal:  1,
		AllowNoMention: 1,
	}))
	for _, uid := range members {
		require.NoError(t, db.InsertMember(&MemberModel{
			GroupNo: groupNo,
			UID:     uid,
			Role:    MemberRoleCommon,
			Status:  int(common.GroupMemberStatusNormal),
		}))
	}
	seedOutboxRow(t, ctx, groupNo)
}

// seedOutboxRow 直接插入一条 outbox 行（channel_type=群）。
func seedOutboxRow(t *testing.T, ctx *config.Context, groupNo string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO group_channel_outbox (group_no, channel_type) VALUES (?, ?)",
		groupNo, int(common.ChannelTypeGroup.Uint8()),
	).Exec()
	require.NoError(t, err)
}

func outboxExists(t *testing.T, ctx *config.Context, groupNo string) bool {
	t.Helper()
	var n int
	err := ctx.DB().SelectBySql("SELECT COUNT(*) FROM group_channel_outbox WHERE group_no=?", groupNo).LoadOne(&n)
	require.NoError(t, err)
	return n > 0
}

func outboxAttempts(t *testing.T, ctx *config.Context, groupNo string) int {
	t.Helper()
	var n int
	err := ctx.DB().SelectBySql("SELECT attempts FROM group_channel_outbox WHERE group_no=?", groupNo).LoadOne(&n)
	require.NoError(t, err)
	return n
}

// newTestReconciler 构造一个使用真实测试 DB、但 IM 创建与订阅者来源均可注入的 reconciler。
// grace 设为负值，使刚插入的 outbox 行立即被判为「到期」。
func newTestReconciler(ctx *config.Context, createChannel func(*config.ChannelCreateReq) error) *channelReconciler {
	r := newChannelReconciler(ctx)
	r.grace = -time.Hour // 让所有 outbox 行立即到期，避免测试等待真实 2min 宽限期
	if createChannel != nil {
		r.createChannel = createChannel
	}
	return r
}

// Test_Issue394_ReconcileRecoversOrphanGroupChannel 验收①/③（失败模式：IM-fail + 补偿删除失败，
// 以及 commit 后崩溃）：孤儿群被 reconcile 用当前成员幂等重建频道并收口，group 行完好保留。
func Test_Issue394_ReconcileRecoversOrphanGroupChannel(t *testing.T) {
	_, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))

	const groupNo = "g394_recover"
	seedOrphanGroup(t, ctx, groupNo, "u1", "u2")

	var gotReq *config.ChannelCreateReq
	r := newTestReconciler(ctx, func(req *config.ChannelCreateReq) error {
		gotReq = req
		return nil
	})

	found, resolved, failed, err := r.reconcileOnce()
	require.NoError(t, err)
	require.Equal(t, 1, found)
	require.Equal(t, 1, resolved)
	require.Equal(t, 0, failed)

	// 频道用群的当前成员重建（幂等 upsert）。
	require.NotNil(t, gotReq, "reconcile 必须为孤儿群调用 IM 频道创建")
	require.Equal(t, groupNo, gotReq.ChannelID)
	require.Equal(t, common.ChannelTypeGroup.Uint8(), gotReq.ChannelType)
	require.ElementsMatch(t, []string{"u1", "u2"}, gotReq.Subscribers)

	// 收口后 outbox 行删除；group 行仍在（绝不删掉有成员的群）。
	require.False(t, outboxExists(t, ctx, groupNo), "频道确认后 outbox 行必须删除")
	g, err := NewDB(ctx).QueryWithGroupNo(groupNo)
	require.NoError(t, err)
	require.NotNil(t, g, "reconcile 必须保留 group 行")
}

// Test_Issue394_ReconcileRetriesWhileIMDown 验收①/②：IM 仍不可用时保留 outbox 行并累加尝试次数
// （可观测退避），IM 恢复后下一轮幂等收口 —— 最终一致，孤儿不会永久残留。
func Test_Issue394_ReconcileRetriesWhileIMDown(t *testing.T) {
	_, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))

	const groupNo = "g394_retry"
	seedOrphanGroup(t, ctx, groupNo, "u1")

	// 第一轮：IM 仍挂。
	imDown := true
	r := newTestReconciler(ctx, func(req *config.ChannelCreateReq) error {
		if imDown {
			return errors.New("wukongim unavailable")
		}
		return nil
	})

	found, resolved, failed, err := r.reconcileOnce()
	require.NoError(t, err)
	require.Equal(t, 1, found)
	require.Equal(t, 0, resolved)
	require.Equal(t, 1, failed)
	require.True(t, outboxExists(t, ctx, groupNo), "IM 未恢复时 outbox 行必须保留待重试")
	require.Equal(t, 1, outboxAttempts(t, ctx, groupNo), "失败一次应累加 attempts")

	// 第二轮：IM 恢复 → 收口。
	imDown = false
	found, resolved, failed, err = r.reconcileOnce()
	require.NoError(t, err)
	require.Equal(t, 1, found)
	require.Equal(t, 1, resolved)
	require.Equal(t, 0, failed)
	require.False(t, outboxExists(t, ctx, groupNo), "IM 恢复后应幂等收口并删除 outbox 行")
}

// Test_Issue394_ReconcileCleansStaleOutboxWhenGroupGone 反向局部失败：补偿删除删掉了 group/成员，
// 但 outbox 删除失败留下陈旧 outbox 行。reconcile 发现 group 已不存在 → 不建频道，直接清理该行。
func Test_Issue394_ReconcileCleansStaleOutboxWhenGroupGone(t *testing.T) {
	_, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))

	const groupNo = "g394_stale"
	seedOutboxRow(t, ctx, groupNo) // 只有 outbox 行，没有 group 行

	called := false
	r := newTestReconciler(ctx, func(req *config.ChannelCreateReq) error {
		called = true
		return nil
	})

	found, resolved, failed, err := r.reconcileOnce()
	require.NoError(t, err)
	require.Equal(t, 1, found)
	require.Equal(t, 1, resolved)
	require.Equal(t, 0, failed)
	require.False(t, called, "group 已不存在时不应重建频道")
	require.False(t, outboxExists(t, ctx, groupNo), "陈旧 outbox 行必须被清理")
}

// Test_Issue394_ReconcileSkipsInFlightWithinGrace 宽限期保护：正常建群在 commit 与 IM 创建之间
// 短暂留有的 outbox 行（updated_at≈now）不被误当孤儿处理。
func Test_Issue394_ReconcileSkipsInFlightWithinGrace(t *testing.T) {
	_, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))

	const groupNo = "g394_inflight"
	seedOrphanGroup(t, ctx, groupNo, "u1")

	called := false
	r := newChannelReconciler(ctx) // 真实宽限期 2min
	r.createChannel = func(req *config.ChannelCreateReq) error { called = true; return nil }

	found, resolved, failed, err := r.reconcileOnce()
	require.NoError(t, err)
	require.Equal(t, 0, found, "宽限期内的 in-flight outbox 行不应被扫到")
	require.Equal(t, 0, resolved)
	require.Equal(t, 0, failed)
	require.False(t, called, "宽限期内不应触发频道重建")
	require.True(t, outboxExists(t, ctx, groupNo), "in-flight outbox 行必须保留")
}
