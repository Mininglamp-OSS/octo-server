package file

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// file 包的 init() 必须把有效上传限制注册给 modules/common 的 appconfig。
//
// 刻意做成纯单测：modules/file 此前没有任何 testutil.NewTestServer 用例，
// 整个包不依赖 MySQL/Redis/WuKongIM。为验证一个 provider 注册就把它变成
// infra 依赖包不划算 —— appconfig 两个响应分支的下发由
// modules/common/api_appconfig_file_limits_test.go 用 fake provider 覆盖，
// 那个包本来就有 infra 依赖。两边合起来覆盖完整链路：
// 「file 注册的值是对的」+「common 在两个分支都下发」。

func TestInit_RegistersFileUploadLimitsProvider(t *testing.T) {
	resetPolicyForTest(t)

	maxKB, exts, ok := common.FileUploadLimits()
	require.True(t, ok, "modules/file 的 init() 必须注册 provider，否则 appconfig 整个字段不下发")

	assert.Equal(t, common.DefaultFileMaxSizeKB, maxKB)
	assert.NotEmpty(t, exts, "空 allowed_extensions 会被客户端读成「什么都不能传」")
	assert.Contains(t, exts, ".jpg")
	assert.Contains(t, exts, ".pdf")
	for _, ext := range []string{".exe", ".php", ".sh"} {
		assert.NotContains(t, exts, ext, "内置黑名单项绝不能出现在下发清单里")
	}
}

// provider 必须反映当前策略快照，而不是注册时刻的固定值 —— 运营封堵一个扩展名
// 后，下发清单要跟着收窄。
func TestInit_ProviderReflectsCurrentPolicy(t *testing.T) {
	resetPolicyForTest(t)
	_, before, ok := common.FileUploadLimits()
	require.True(t, ok)
	require.Contains(t, before, ".pdf")

	SetPolicySettings(fakePolicySettings{blocked: []string{".pdf"}, maxKB: 2048})
	cached.Store(nil)

	maxKB, after, ok := common.FileUploadLimits()
	require.True(t, ok)
	assert.Equal(t, 2048, maxKB)
	assert.NotContains(t, after, ".pdf", "封堵后下发清单必须收窄")
	assert.Contains(t, after, ".jpg")
}
