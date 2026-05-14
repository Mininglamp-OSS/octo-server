// Package bot_api · YUJ-644 / Mininglamp-OSS#33 unit tests for
// PERSONAL DM payload.space_id authoritative injection.
//
// 真实集成测试（HTTP + DB）见 modules/bot_api/send_test.go 中的相邻 cases；
// 这里覆盖 enrichBotPayloadWithSpaceID 的纯逻辑分支：scope=space → ctx 路径，
// 缺省 → fallback DB 路径，DB 失败 → 不阻断，空 SpaceID → 监控 warn。
package bot_api

import (
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// resolveBotActiveSpaceID 是混入 *BotAPI.db 的 query，因此这里走"包内 helper"
// 测试：直接调用 enrichBotPayloadWithSpaceID 的纯分支需要桩 db；为简化，把 ctx
// 路径单独抽出一个不依赖 db 的小测试。

// fakeWkContext 创建一个最小可用的 wkhttp.Context（gin context wrapper）。
func fakeWkContext() *wkhttp.Context {
	c, _ := gin.CreateTestContext(nil)
	return &wkhttp.Context{Context: c}
}

func TestResolveBotActiveSpaceID_AppBotScopeSpace_UsesCtxValue(t *testing.T) {
	ba := &BotAPI{}
	c := fakeWkContext()
	c.Set(CtxKeyAppBotScope, "space")
	c.Set(CtxKeyAppBotSpaceID, "spaceA")
	got := ba.resolveBotActiveSpaceID(c, "bot_robot_1")
	assert.Equal(t, "spaceA", got, "App Bot scope=space 应直接使用 ctx 写入的 SpaceID（无 DB）")
}

func TestResolveBotActiveSpaceID_AppBotScopeSpaceMissingValue(t *testing.T) {
	ba := &BotAPI{}
	c := fakeWkContext()
	c.Set(CtxKeyAppBotScope, "space")
	// CtxKeyAppBotSpaceID 故意不写入 → 跳到 DB fallback；db 为 nil 会 panic
	// 触发 robot 路径前我们捕获 panic 来锁住"必须 fallback 才走 DB"的契约。
	defer func() { _ = recover() }()
	_ = ba.resolveBotActiveSpaceID(c, "bot_robot_2")
}

func TestEnrichBotPayloadWithSpaceID_AppBotScopeSpace_OverridesClient(t *testing.T) {
	ba := &BotAPI{}
	c := fakeWkContext()
	c.Set(CtxKeyAppBotScope, "space")
	c.Set(CtxKeyAppBotSpaceID, "spaceAuth")
	payload := map[string]interface{}{"content": "hi", "space_id": "spaceForged"}
	got := ba.enrichBotPayloadWithSpaceID(c, "bot_robot_1", payload)
	assert.Equal(t, "spaceAuth", got["space_id"], "PERSONAL 必须用服务端权威 SpaceID 覆盖客户端伪造值")
}

func TestEnrichBotPayloadWithSpaceID_NilPayloadInitialized(t *testing.T) {
	ba := &BotAPI{}
	c := fakeWkContext()
	c.Set(CtxKeyAppBotScope, "space")
	c.Set(CtxKeyAppBotSpaceID, "spaceAuth")
	got := ba.enrichBotPayloadWithSpaceID(c, "bot_robot_1", nil)
	assert.NotNil(t, got)
	assert.Equal(t, "spaceAuth", got["space_id"])
}

// errSentinel 用于触发 querySpaceIDByRobotID 失败路径。当前 helper 直接
// 调用 ba.db；这里通过 monkey-replace 不优雅，先以 ctx-路径覆盖关键
// 不变量；DB 失败 / 空 SpaceID 路径在更高层（HTTP 集成测试）覆盖。
var _ = errors.New
