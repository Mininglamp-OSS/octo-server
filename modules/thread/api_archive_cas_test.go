package thread

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestArchiveThreadAPI_SucceedsWhenRowVersionAheadOfGenSeq 端到端回归，复现线上
// 2026-11-06 的 400 `update thread status: thread CAS retry exhausted`：
// 多副本部署下，行内 version 已被另一个副本（更大的 GenSeq 段）推高，本进程
// GenSeq 生成的值更小，旧实现三次重试全部失败 → classifyThreadError 兜底成
// 500 store_failed → 前端「归档失败 / 未知错误」。
//
// 这里把行 version 置成远大于进程内 GenSeq（约 1e6）的值，再走真实 HTTP 路由，
// 断言归档返回 200 且 version 严格递增。
func TestArchiveThreadAPI_SucceedsWhenRowVersionAheadOfGenSeq(t *testing.T) {
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)
	db := NewDB(ctx)

	const (
		shortID     = "2118288744321650688"
		hugeVersion = int64(1_600_000_000_000) // 远大于进程内 GenSeq（~1e6）
	)
	require.NoError(t, db.Insert(&Model{
		ShortID:    shortID,
		GroupNo:    groupNo,
		Name:       "version-ahead",
		CreatorUID: testutil.UID,
		Status:     ThreadStatusActive,
		Version:    hugeVersion,
	}))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/groups/"+groupNo+"/threads/"+shortID+"/archive", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code, "archive must not fail on CAS; body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), `"status":200`)

	m, err := db.QueryByShortID(shortID)
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, ThreadStatusArchived, m.Status)
	assert.Equal(t, hugeVersion+1, m.Version, "version must advance past the row value, never regress")
}
