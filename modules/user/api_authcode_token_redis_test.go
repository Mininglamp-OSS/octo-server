package user

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PR #225 R3 Blocking (yujiawei) 的真实 Redis 集成回归。
//
// resurrection 漏洞的本质是对 token:<oldToken> 的 Redis 写:扫码登录(loginWithAuthCode)
// 复用 uidtoken 反查到的旧 token 时,必须用 SET XX(仅当 key 仍存在才写),否则会把并发
// OIDC logout 刚删掉的 token 复活。这里用生产路径的 redisExistingTokenSetter(真实
// go-redis SET XX)直接验证两条不变量;loginWithAuthCode 确实在调 IM 之前走这个 setter,
// 由源码锁 TestLoginWithAuthCode_ReusedTokenGuardedBySetXX 保证。
//
// 只依赖 Redis(CI 必备),不碰 WuKongIM —— 完整 HTTP e2e 因 CI 的 IM 不接受给未注册 uid
// 发 token(UpdateIMToken 返回 im_call_failed)而不可移植,故下沉到 race 真正发生的
// Redis 层。配套 fake 实现的纯单测见 TestRefreshExistingLoginToken_DoesNotRecreateDeletedToken。
func TestRedisExistingTokenSetter_SetXX_RealRedis(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	setter := redisExistingTokenSetter{
		client: rd.NewClient(octoredis.MustBuildOptions(ctx.GetConfig())),
	}
	prefix := ctx.GetConfig().Cache.TokenCachePrefix

	t.Run("key不存在_不创建_模拟logout已删除", func(t *testing.T) {
		key := prefix + "pr225-missing-" + util.GenerUUID()
		require.NoError(t, ctx.GetRedisConn().Del(key)) // 前置:token:<old> 已被 logout 删除
		ok, err := setter.SetIfExists(key, "payload", time.Minute)
		require.NoError(t, err)
		assert.False(t, ok, "SET XX 对不存在的 key 必须返回 false,不能复活已登出 token")
		got, err := ctx.Cache().Get(key)
		require.NoError(t, err)
		assert.Empty(t, got, "已登出的 token key 不得被重新创建")
	})

	t.Run("key存在_刷新payload_保证Web多端复用", func(t *testing.T) {
		key := prefix + "pr225-exists-" + util.GenerUUID()
		require.NoError(t, ctx.Cache().SetAndExpire(key, "old-payload", time.Minute))
		ok, err := setter.SetIfExists(key, "new-payload", time.Minute)
		require.NoError(t, err)
		assert.True(t, ok, "SET XX 对存在的 key 必须返回 true(正常 Web 多端复用)")
		got, err := ctx.Cache().Get(key)
		require.NoError(t, err)
		assert.Equal(t, "new-payload", got, "SET XX 必须刷新已存在 key 的 payload")
	})
}
