package space

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// imStub 是一个可切换成功/失败的 WuKongIM 桩。
// 失败分支是本次修复的**唯一**危险分支，用真 broker 造不出来。
type imStub struct {
	mu     sync.Mutex
	fail   bool
	calls  []subRemoveCall
	server *httptest.Server
}

type subRemoveCall struct {
	ChannelID   string   `json:"channel_id"`
	ChannelType uint8    `json:"channel_type"`
	Subscribers []string `json:"subscribers"`
}

func newIMStub(t *testing.T, ctx *config.Context) *imStub {
	t.Helper()
	stub := &imStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/channel/subscriber_remove", func(w http.ResponseWriter, r *http.Request) {
		var call subRemoveCall
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &call)
		stub.mu.Lock()
		failing := stub.fail
		if !failing {
			stub.calls = append(stub.calls, call)
		}
		stub.mu.Unlock()
		if failing {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"msg":"broker down"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) })
	stub.server = httptest.NewServer(mux)

	cfg := ctx.GetConfig()
	previous := cfg.WuKongIM.APIURL
	cfg.WuKongIM.APIURL = stub.server.URL
	t.Cleanup(func() {
		cfg.WuKongIM.APIURL = previous
		stub.server.Close()
	})
	return stub
}

func (s *imStub) setFail(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = v
}

func (s *imStub) succeeded(channelID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range s.calls {
		if c.ChannelID == channelID {
			out = append(out, c.Subscribers...)
		}
	}
	return out
}

func pendingIMRows(t *testing.T, channelID string) []struct {
	UID       string `db:"uid"`
	Status    uint8  `db:"status"`
	Attempts  uint32 `db:"attempts"`
	LastError string `db:"last_error"`
} {
	t.Helper()
	var rows []struct {
		UID       string `db:"uid"`
		Status    uint8  `db:"status"`
		Attempts  uint32 `db:"attempts"`
		LastError string `db:"last_error"`
	}
	_, err := testCtx.DB().SelectBySql(
		"SELECT uid, status, attempts, last_error FROM im_pending_subscriber_removal WHERE channel_id=? ORDER BY id",
		channelID).Load(&rows)
	require.NoError(t, err)
	return rows
}

// TestIMUnsubscribeFailureLeavesPendingRecord 退订失败必须留下待办。
//
// 这是整条链路的入口。原先 modules/group/service.go:1912 只打日志，失败就此蒸发，
// 工单照样标 done —— 被移除的人永久保留完整群成员权限（实测：照发照收）。
func TestIMUnsubscribeFailureLeavesPendingRecord(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)
	stub := newIMStub(t, f.ctx)
	stub.setFail(true)

	const ch = "g-fail-leaves-row"
	require.NoError(t, EnqueueIMUnsubscribe(f.ctx.DB(), ch, 2, []string{"u-left"}))
	_ = AttemptIMUnsubscribe(f.ctx, ch, 2, []string{"u-left"})

	rows := pendingIMRows(t, ch)
	require.Len(t, rows, 1, "退订失败必须留下待办行，不能只打日志")
	assert.Equal(t, "u-left", rows[0].UID)
	assert.Equal(t, removalCleanupPending, rows[0].Status)
}

// TestIMUnsubscribeSuccessLeavesNoRecord 顺利路径不留痕：成功即删行。
// 一次 1000 人 / 50 群的解散是约 5 万次退订，全部留痕只会压垮保留期清理。
func TestIMUnsubscribeSuccessLeavesNoRecord(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)
	stub := newIMStub(t, f.ctx)

	const ch = "g-ok-no-row"
	require.NoError(t, EnqueueIMUnsubscribe(f.ctx.DB(), ch, 2, []string{"u-ok"}))
	require.NoError(t, AttemptIMUnsubscribe(f.ctx, ch, 2, []string{"u-ok"}))

	assert.Empty(t, pendingIMRows(t, ch), "成功后不得留下任何行")
	assert.Equal(t, []string{"u-ok"}, stub.succeeded(ch))
}

// TestIMPendingWorkerDrainsAfterBrokerRecovers broker 恢复后，待办必须被排掉。
func TestIMPendingWorkerDrainsAfterBrokerRecovers(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)
	stub := newIMStub(t, f.ctx)
	stub.setFail(true)

	const ch = "g-drain"
	require.NoError(t, EnqueueIMUnsubscribe(f.ctx.DB(), ch, 2, []string{"u-drain"}))
	_ = AttemptIMUnsubscribe(f.ctx, ch, 2, []string{"u-drain"})
	require.Len(t, pendingIMRows(t, ch), 1)

	stub.setFail(false)
	f.processIMPendingRemovals()

	assert.Empty(t, pendingIMRows(t, ch), "broker 恢复后待办必须排空")
	assert.Equal(t, []string{"u-drain"}, stub.succeeded(ch))
}

// TestIMPendingRetryDoesNotDependOnGroupMember 是本次修复的**核心**回归。
//
// 今天重跑清理工单毫无用处，正是因为重试范围由活跃 group_member 行推导：人已经删了，
// 范围是空集，重跑是空转，还会把一次真实故障洗成 done。待办行必须自带全部信息
// （channel + uid），在业务库里一行成员记录都不存在的情况下照样能排掉。
func TestIMPendingRetryDoesNotDependOnGroupMember(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)
	stub := newIMStub(t, f.ctx)

	const ch = "g-no-member-row"
	// 整个库里没有任何 group_member / space_member 行与之对应。
	require.NoError(t, EnqueueIMUnsubscribe(f.ctx.DB(), ch, 2, []string{"u-ghost"}))

	f.processIMPendingRemovals()

	assert.Empty(t, pendingIMRows(t, ch), "待办必须自足，不能依赖已被删除的成员行")
	assert.Equal(t, []string{"u-ghost"}, stub.succeeded(ch))
}

// TestIMPendingHandlesThreadChannels 子区频道走同一条路。
// 子区 channelID 形如 {groupNo}____{shortID}，channel_type 与父群不同，
// 但对待办表而言只是另一个 (channel_id, channel_type)。
func TestIMPendingHandlesThreadChannels(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)
	stub := newIMStub(t, f.ctx)

	const threadCh = "g-parent____ab12"
	const threadType uint8 = 3
	require.NoError(t, EnqueueIMUnsubscribe(f.ctx.DB(), threadCh, threadType, []string{"u-thread"}))

	f.processIMPendingRemovals()

	assert.Empty(t, pendingIMRows(t, threadCh))
	assert.Equal(t, []string{"u-thread"}, stub.succeeded(threadCh))
}

// TestIMPendingAbandonsAfterMaxAttempts 永久失败要走到 abandoned，而不是无限重试。
func TestIMPendingAbandonsAfterMaxAttempts(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)
	stub := newIMStub(t, f.ctx)
	stub.setFail(true)

	const ch = "g-abandon"
	require.NoError(t, EnqueueIMUnsubscribe(f.ctx.DB(), ch, 2, []string{"u-doomed"}))
	_, err = testCtx.DB().Exec(
		"UPDATE im_pending_subscriber_removal SET attempts=? WHERE channel_id=?",
		removalCleanupMaxAttempts-1, ch)
	require.NoError(t, err)

	f.processIMPendingRemovals()

	rows := pendingIMRows(t, ch)
	require.Len(t, rows, 1, "abandoned 行必须留下来供运维查看")
	assert.Equal(t, removalCleanupAbandoned, rows[0].Status)
	assert.Equal(t, removalCleanupMaxAttempts, rows[0].Attempts)
}

// TestEnqueueIMUnsubscribeIsIdempotent 同一个 (channel, uid) 重复入队不产生重复行。
// 重试路径会反复入队，唯一键必须把它折叠掉，否则一次 broker 抖动就能刷出成千条重复。
func TestEnqueueIMUnsubscribeIsIdempotent(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)
	newIMStub(t, f.ctx)

	const ch = "g-idem"
	require.NoError(t, EnqueueIMUnsubscribe(f.ctx.DB(), ch, 2, []string{"u-dup"}))
	require.NoError(t, EnqueueIMUnsubscribe(f.ctx.DB(), ch, 2, []string{"u-dup"}))
	require.NoError(t, EnqueueIMUnsubscribe(f.ctx.DB(), ch, 2, []string{"u-dup", "u-other"}))

	rows := pendingIMRows(t, ch)
	assert.Len(t, rows, 2, "重复入队必须折叠成一行，第二个 uid 另起一行")
}

// TestIMUnsubscribeHandlesMultipleUIDs 批量路径：入队与「成功即删」都必须能处理多个 uid。
//
// RemoveGroupMembers 一次可以踢到 200 人（managerMaxBatchUIDs），解散更是逐群成百上千次。
// 入队走的是一条多行 INSERT，删除走的是 uid IN (...)；任一侧只处理了第一个 uid，
// 剩下的人就会静静留在群里，而单 uid 的用例永远发现不了。
func TestIMUnsubscribeHandlesMultipleUIDs(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)
	stub := newIMStub(t, f.ctx)

	const ch = "g-multi"
	uids := []string{"u-m1", "u-m2", "u-m3"}

	stub.setFail(true)
	require.NoError(t, EnqueueIMUnsubscribe(f.ctx.DB(), ch, 2, uids))
	_ = AttemptIMUnsubscribe(f.ctx, ch, 2, uids)
	require.Len(t, pendingIMRows(t, ch), 3, "三个 uid 必须各留一行待办")

	stub.setFail(false)
	require.NoError(t, AttemptIMUnsubscribe(f.ctx, ch, 2, uids))
	assert.Empty(t, pendingIMRows(t, ch), "成功后三行必须全删，不能只删第一个")
}
