package common

// =============================================================================
// 偏序守卫的写竞态（task per-user-hiding-window / P0-A，结转自 PR #679 review）
//
// 守卫原先对着**进程本地快照**（60s TTL，只有写入方立即 Reload）校验，且校验发生在
// 事务之外 —— check 与 write 只是相邻，不是原子。于是从 {enabled, archive=6, recent=3}
// 出发，A 写 archive=4、B 写 recent=5，各自对着同一份旧视图都通过（4>=3、6>=5），
// 双双提交，落到 4 < 5：正是守卫本要禁止的状态。
//
// PR #679 把它判为 P2 放行，理由是「需要两个 SuperAdmin 在窗口内写两个不同的 key」。
// 本批把窗口交给用户，写入面从少数管理调用放大到每用户可写，那个理由随之失效 ——
// 所以这是 Batch 2 的准入门，必须先关。
//
// 依赖 MySQL + Redis + WuKongIM（testutil.NewTestServer）。
// =============================================================================

import (
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedOrdering 把三个 key 写成给定的起始状态（直接走 DB，绕开管理接口的守卫）。
func seedOrdering(t *testing.T, s *SystemSettings, enabled, archiveDays, recentDays string) {
	t.Helper()
	require.NoError(t, s.db.upsert("thread", "auto_archive_enabled", enabled, settingTypeBool, ""))
	require.NoError(t, s.db.upsert("thread", "auto_archive_days", archiveDays, settingTypeInt, ""))
	require.NoError(t, s.db.upsert("sidebar", "recent_filter_thread_days", recentDays, settingTypeInt, ""))
	require.NoError(t, s.Reload())
}

// finalOrdering 从 DB 重新解析终态，不信任进程快照。
func finalOrdering(t *testing.T, s *SystemSettings) ThreadArchiveOrdering {
	t.Helper()
	require.NoError(t, s.Reload())
	return s.ThreadArchiveOrdering()
}

// ---------------------------------------------------------------------------
// 核心回归：并发的两个单键写入不得双双提交出违规终态
//
// 修复前这条必失败 —— 两个请求都会 200，终态 4 < 5。修复后守卫在锚点行锁内重读，
// 后到的那个看到的是对方已提交的值，于是被拒。
// ---------------------------------------------------------------------------

func TestManagerSystemSetting_OrderingSurvivesConcurrentSingleKeyWrites(t *testing.T) {
	route, ctx := newSuperAdminServer(t)
	s := EnsureSystemSettings(ctx)

	// 起点合法：归档 6 天 >= 隐藏 3 天。
	seedOrdering(t, s, "1", "6", "3")

	// A：把归档收紧到 4（对旧视图 4>=3 合法）
	// B：把隐藏放宽到 5（对旧视图 6>=5 合法）
	// 两者合并后 4 < 5 —— 必须恰好一个成功。
	var wg sync.WaitGroup
	codes := make([]int, 2)
	bodies := make([]string, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		w := postSystemSetting(t, route,
			`{"items":[{"category":"thread","key":"auto_archive_days","value":"4"}]}`)
		codes[0], bodies[0] = w.Code, w.Body.String()
	}()
	go func() {
		defer wg.Done()
		w := postSystemSetting(t, route,
			`{"items":[{"category":"sidebar","key":"recent_filter_thread_days","value":"5"}]}`)
		codes[1], bodies[1] = w.Code, w.Body.String()
	}()
	wg.Wait()

	okCount := 0
	for _, c := range codes {
		if c == http.StatusOK {
			okCount++
		}
	}
	assert.Equal(t, 1, okCount,
		"恰好一个写入应当成功；两个都成功说明守卫仍在事务外对着旧快照校验。codes=%v bodies=%v",
		codes, bodies)

	got := finalOrdering(t, s)
	assert.False(t, ViolatesThreadArchiveOrdering(got),
		"DB 终态不得违反偏序，实际 archive=%d recent=%d", got.ArchiveDays, got.RecentDays)
}

// 同样的竞态，但起点是**没有任何 DB 行**（回落 env/默认）—— 这是全新部署的形状，
// 也是「FOR UPDATE 锁不住不存在的行」那条验收针对的场景：守卫锁的是必然存在的锚点行，
// 所以三个 setting 行是否存在与串行化无关。
func TestManagerSystemSetting_OrderingSerialisesWhenNoRowsExist(t *testing.T) {
	route, ctx := newSuperAdminServer(t)
	s := EnsureSystemSettings(ctx)

	t.Setenv(envThreadAutoArchiveEnabled, "true")
	t.Setenv(envThreadAutoArchiveDays, "6")
	require.NoError(t, s.Reload()) // 无任何 thread.* / sidebar.* 行

	_, hasRow := s.lookup("thread", "auto_archive_days")
	require.False(t, hasRow,
		"前提：这条路径上不存在 DB 行，守卫无法靠锁 setting 行来串行化")

	var wg sync.WaitGroup
	codes := make([]int, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		codes[0] = postSystemSetting(t, route,
			`{"items":[{"category":"thread","key":"auto_archive_days","value":"4"}]}`).Code
	}()
	go func() {
		defer wg.Done()
		codes[1] = postSystemSetting(t, route,
			`{"items":[{"category":"sidebar","key":"recent_filter_thread_days","value":"5"}]}`).Code
	}()
	wg.Wait()

	got := finalOrdering(t, s)
	assert.False(t, ViolatesThreadArchiveOrdering(got),
		"无 DB 行起步时终态同样不得违反偏序，实际 archive=%d recent=%d codes=%v",
		got.ArchiveDays, got.RecentDays, codes)
}

// 守卫改为在事务内校验之后，被拒的那一批**不得留下任何部分写入** —— 同批里合法的
// 其它 key 也要一起回滚。修复前守卫在 beginTx 之前 return，天然没有这个风险；
// 现在它在事务里 return，回滚路径就必须被钉住。
func TestManagerSystemSetting_OrderingRejectionRollsBackWholeBatch(t *testing.T) {
	route, ctx := newSuperAdminServer(t)
	s := EnsureSystemSettings(ctx)
	seedOrdering(t, s, "1", "6", "3")

	// register.off 与偏序无关且取值合法；但同批的 recent=30 违反 6>=30，整批必须回滚。
	w := postSystemSetting(t, route, `{"items":[
		{"category":"register","key":"off","value":"1"},
		{"category":"sidebar","key":"recent_filter_thread_days","value":"30"}
	]}`)
	require.NotEqual(t, http.StatusOK, w.Code, "违反偏序必须拒绝：%s", w.Body.String())

	require.NoError(t, s.Reload())
	assert.False(t, s.RegisterOff(),
		"同批中与偏序无关的 register.off 不得被写入——守卫在事务内拒绝时必须整批回滚")
	assert.Equal(t, 3, s.SidebarRecentFilterThreadDays(), "隐藏窗口应保持原值")
}

// 不触及三个 key 的批次不该被守卫牵连：既不加锁也不多读，行为与从前一致。
func TestManagerSystemSetting_UnrelatedBatchSkipsGuard(t *testing.T) {
	route, ctx := newSuperAdminServer(t)
	s := EnsureSystemSettings(ctx)
	seedOrdering(t, s, "1", "6", "3")

	w := postSystemSetting(t, route,
		`{"items":[{"category":"register","key":"off","value":"1"}]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	require.NoError(t, s.Reload())
	assert.True(t, s.RegisterOff())
}

// ---------------------------------------------------------------------------
// 事务内解析器：必须与快照 getter 逐条同义
//
// 守卫现在读的是锁内的原始行，而不是快照。两条路径对「行缺失 / 空串 / 越界 / 非法」
// 的理解一旦分叉，守卫校验的就不是写入后真正生效的状态。
// ---------------------------------------------------------------------------

func TestResolveThreadArchiveOrderingFrom_MirrorsSnapshotGetters(t *testing.T) {
	t.Setenv(envThreadAutoArchiveEnabled, "true")
	t.Setenv(envThreadAutoArchiveDays, "9999") // env 层无上界

	cases := []struct {
		name string
		rows map[string]string
	}{
		{"no rows at all", map[string]string{}},
		{"empty values are not-configured", map[string]string{
			"thread.auto_archive_enabled":       "",
			"thread.auto_archive_days":          "",
			"sidebar.recent_filter_thread_days": "",
		}},
		{"configured values win", map[string]string{
			"thread.auto_archive_enabled":       "0",
			"thread.auto_archive_days":          "30",
			"sidebar.recent_filter_thread_days": "7",
		}},
		{"out-of-range int falls back", map[string]string{
			"thread.auto_archive_days":          "99999", // > settingIntMax
			"sidebar.recent_filter_thread_days": "-5",
		}},
		{"unparseable int falls back", map[string]string{
			"thread.auto_archive_days": "abc",
		}},
		{"unparseable bool falls back to env", map[string]string{
			"thread.auto_archive_enabled": "maybe",
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTestSystemSettings(t, nil)
			for k, v := range c.rows {
				cat, key := splitSchemaKeyForTest(k)
				require.NoError(t, s.db.upsert(cat, key, v, settingTypeString, ""))
			}
			require.NoError(t, s.Reload())

			want := s.ThreadArchiveOrdering()
			got := ResolveThreadArchiveOrderingFrom(c.rows)
			assert.Equal(t, want, got,
				"事务内解析必须与快照 getter 给出同一个结论，否则守卫校验的不是写入后生效的状态")
		})
	}
}

func splitSchemaKeyForTest(k string) (string, string) {
	for i := 0; i < len(k); i++ {
		if k[i] == '.' {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}
