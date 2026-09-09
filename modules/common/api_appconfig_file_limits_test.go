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

// appconfig 下发文件上传限制的**降级行为**（task file-extension-policy-dynamic-config）。
//
// 本包的测试二进制不链接 modules/file（common 不能 import file），所以 provider
// 天然处于未注册状态 —— 正好覆盖降级路径。真实链接下的双分支下发由
// modules/file/appconfig_file_limits_test.go 覆盖，那里 file 的 init() 会注册
// provider。

// provider 未注册时整个字段不下发，而不是下发一个空数组 ——
// 空 allowed_extensions 会被客户端读成「什么都不能传」，比缺字段更危险。
func TestGetAppConfig_FileUploadLimitsOmittedWhenProviderMissing(t *testing.T) {
	prev := fileUploadLimitsProvider.Load()
	fileUploadLimitsProvider.Store(nil)
	t.Cleanup(func() { fileUploadLimitsProvider.Store(prev) })

	s, ctx := testutil.NewTestServer()
	cn := New(ctx)
	cleanAllTablesAndReloadSettings(t, ctx)
	require.NoError(t, cn.appConfigDB.insert(&appConfigModel{}))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/v1/common/appconfig", nil)
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	_, present := body["file_upload_limits"]
	assert.False(t, present,
		"provider 未注册时必须整体省略该字段：%s", w.Body.String())
}

// 两个响应分支都必须下发 file_upload_limits。handler 有一条 version 短路分支，
// 老客户端命中它时若拿不到最新值，运维在管理台调整后会被客户端缓存住 ——
// 与 sticker_upload_limits / DocsOn 的处理理由相同。
//
// 用 fake provider 注入：真实值的正确性由 modules/file 侧的纯单测覆盖，这里
// 验证的是 common 的下发链路（两个分支 + 字段形状）。
func TestGetAppConfig_FileUploadLimitsInEveryResponseBranch(t *testing.T) {
	for _, path := range []string{
		"/v1/common/appconfig",
		"/v1/common/appconfig?version=99999999",
	} {
		t.Run(path, func(t *testing.T) {
			prev := fileUploadLimitsProvider.Load()
			t.Cleanup(func() { fileUploadLimitsProvider.Store(prev) })
			SetFileUploadLimitsProvider(func() (int, []string) {
				return 4096, []string{".jpg", ".pdf"}
			})

			s, ctx := testutil.NewTestServer()
			cn := New(ctx)
			cleanAllTablesAndReloadSettings(t, ctx)
			require.NoError(t, cn.appConfigDB.insert(&appConfigModel{Version: 1}))

			w := httptest.NewRecorder()
			req, _ := http.NewRequest(http.MethodGet, path, nil)
			s.GetRoute().ServeHTTP(w, req)
			require.Equal(t, http.StatusOK, w.Code)

			var body struct {
				FileUploadLimits *struct {
					MaxSizeKB         int      `json:"max_size_kb"`
					AllowedExtensions []string `json:"allowed_extensions"`
				} `json:"file_upload_limits"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			require.NotNil(t, body.FileUploadLimits,
				"两个分支都必须下发 file_upload_limits：%s", w.Body.String())
			assert.Equal(t, 4096, body.FileUploadLimits.MaxSizeKB)
			assert.Equal(t, []string{".jpg", ".pdf"}, body.FileUploadLimits.AllowedExtensions)

			// 只下发 allowed，不下发 blocked：本端点无鉴权，下发黑名单等于让任何
			// 未认证调用方对比 baseline 就看出本部署额外封了哪些扩展名。
			assert.NotContains(t, w.Body.String(), "blocked_extensions")
		})
	}
}
