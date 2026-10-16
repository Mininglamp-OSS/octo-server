package common

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// appconfig 下发 Drive 上传限制（task drive-upload-policy / 设计 §6.2）。
//
// 这是集成测试：走 testutil server + 真实路由 + DB 快照。drive_upload_limits 是
// **值类型恒下发**（不是 file_upload_limits 那种 provider 缺失即省略的指针），
// 因为本字段由 system_setting 恒可计算，不存在「provider 未注册」态（06 冻结
// 决策 4E）。
//
// 与 drive_upload_v2_on 解耦的理由同 sticker_upload_limits：两条响应分支都必须
// 下发 —— 老客户端命中 version 短路分支时若拿不到最新值，运维调整会被客户端缓存
// 住（R2.1 硬要求）。
func TestGetAppConfig_DriveUploadLimitsInEveryResponseBranch(t *testing.T) {
	t.Setenv(envDriveUploadMaxSizeKBHardCap, "") // 默认 10 GiB 天花板
	for _, path := range []string{
		"/v1/common/appconfig",
		"/v1/common/appconfig?version=99999999",
	} {
		t.Run(path, func(t *testing.T) {
			s, ctx := testutil.NewTestServer()
			cn := New(ctx)
			cleanAllTablesAndReloadSettings(t, ctx)
			// version 短路分支要求 DB 里已有一条更大的 version。
			require.NoError(t, cn.appConfigDB.insert(&appConfigModel{Version: 1}))

			w := httptest.NewRecorder()
			req, _ := http.NewRequest(http.MethodGet, path, nil)
			s.GetRoute().ServeHTTP(w, req)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())

			var body struct {
				DriveUploadLimits *struct {
					MaxSizeKB             int `json:"max_size_kb"`
					MultipartThresholdKB  int `json:"multipart_threshold_kb"`
					PartSizeKB            int `json:"part_size_kb"`
					MaxConcurrentParts    int `json:"max_concurrent_parts"`
					MaxConcurrentSessions int `json:"max_concurrent_sessions"`
				} `json:"drive_upload_limits"`
				DriveUploadV2On *bool `json:"drive_upload_v2_on"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			require.NotNil(t, body.DriveUploadLimits,
				"两个分支都必须下发 drive_upload_limits：%s", w.Body.String())
			require.NotNil(t, body.DriveUploadV2On,
				"两个分支都必须下发 drive_upload_v2_on：%s", w.Body.String())

			// 默认值：1 GiB 上限 + 分片默认参数（128 MiB / 64 MiB / 并发 3 / 会话 3）。
			assert.Equal(t, DefaultDriveUploadMaxSizeKB, body.DriveUploadLimits.MaxSizeKB)
			assert.Equal(t, 131072, body.DriveUploadLimits.MultipartThresholdKB)
			assert.Equal(t, 65536, body.DriveUploadLimits.PartSizeKB)
			assert.Equal(t, 3, body.DriveUploadLimits.MaxConcurrentParts)
			assert.Equal(t, 3, body.DriveUploadLimits.MaxConcurrentSessions)
			// 前端灰度开关默认关闭。
			assert.False(t, *body.DriveUploadV2On)

			// Web 侧不下发 policy_version（06 4E / §9-Q6）。
			assert.NotContains(t, w.Body.String(), "policy_version")
		})
	}
}
