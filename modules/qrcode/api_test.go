package qrcode

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-server/modules/group"
	_ "github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

func TestMain(m *testing.M) {
	key := make([]byte, 16)
	rand.Read(key)
	os.Setenv("OCTO_MASTER_KEY", hex.EncodeToString(key)) // 32 hex chars = 32 bytes
	os.Exit(m.Run())
}


func TestHandleJoinGroup_GroupNotFound(t *testing.T) {
	s, ctx := testutil.NewTestServer()

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	code := util.GenerUUID()
	err = ctx.GetRedisConn().SetAndExpire(
		fmt.Sprintf("%s%s", common.QRCodeCachePrefix, code),
		util.ToJson(common.NewQRCodeModel(common.QRCodeTypeGroup, map[string]interface{}{
			"group_no":  "non-existent-group",
			"generator": "10001",
		})),
		time.Minute,
	)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/qrcode/"+code, nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "群不存在")
}

func TestHandleJoinGroup_CrossSpaceAllowedByDefault(t *testing.T) {
	s, ctx := testutil.NewTestServer()

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// 群属于 space1，且允许外部成员（默认）。测试用户 testutil.UID 不在 space1，
	// 预检应放行（真正的外部成员判定和 allow_external 校验由 groupScanJoin 完成）。
	groupDB := group.NewDB(ctx)
	err = groupDB.Insert(&group.Model{
		GroupNo:       "group1",
		Name:          "测试群",
		Creator:       "10001",
		Status:        1,
		SpaceID:       "space1",
		AllowExternal: 1,
	})
	assert.NoError(t, err)

	code := util.GenerUUID()
	err = ctx.GetRedisConn().SetAndExpire(
		fmt.Sprintf("%s%s", common.QRCodeCachePrefix, code),
		util.ToJson(common.NewQRCodeModel(common.QRCodeTypeGroup, map[string]interface{}{
			"group_no":  "group1",
			"generator": "10001",
		})),
		time.Minute,
	)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/qrcode/"+code, nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	// 预检不拦截：返回 200，body 应包含 auth_code（非 Space 成员也走到发 auth_code 的分支）
	assert.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), "请先加入该空间后再扫码入群")
	assert.NotContains(t, w.Body.String(), "该群仅允许本空间成员加入")
	assert.Contains(t, w.Body.String(), "auth_code")
}

func TestHandleJoinGroup_CrossSpaceBlockedWhenAllowExternalOff(t *testing.T) {
	s, ctx := testutil.NewTestServer()

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// 群禁止外部成员加入（allow_external=0），测试用户不在 space1 → 预检拦截。
	groupDB := group.NewDB(ctx)
	err = groupDB.Insert(&group.Model{
		GroupNo:       "group1",
		Name:          "测试群",
		Creator:       "10001",
		Status:        1,
		SpaceID:       "space1",
		AllowExternal: 0,
	})
	assert.NoError(t, err)

	code := util.GenerUUID()
	err = ctx.GetRedisConn().SetAndExpire(
		fmt.Sprintf("%s%s", common.QRCodeCachePrefix, code),
		util.ToJson(common.NewQRCodeModel(common.QRCodeTypeGroup, map[string]interface{}{
			"group_no":  "group1",
			"generator": "10001",
		})),
		time.Minute,
	)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/qrcode/"+code, nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "该群仅允许本空间成员加入")
}
