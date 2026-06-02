package user

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PR #225 R3 Blocking (yujiawei) — 扫码登录(loginWithAuthCode)复用 uidtoken 反查到的
// 旧 token 时,必须先确认 token:<oldToken> 仍存在(SET XX),否则与并发 OIDC logout
// 删除 token 形成 TOCTOU,会把刚 logout 的 HTTP token 复活,绕过登出。
//
// 该 handler 依赖 IM/DB/Redis,CI 单测环境不便端到端跑,这里用源码契约锁(无依赖,
// 一定跑)锁住关键不变量,防止守卫被回归掉。配套的 SET XX 行为单测见
// TestRefreshExistingLoginToken_DoesNotRecreateDeletedToken。
func TestLoginWithAuthCode_ReusedTokenGuardedBySetXX(t *testing.T) {
	src, err := os.ReadFile("api.go")
	require.NoError(t, err)
	body := string(src)

	fnSig := "func (u *User) loginWithAuthCode("
	fnStart := strings.Index(body, fnSig)
	require.NotEqual(t, -1, fnStart, "loginWithAuthCode handler 必须存在")
	fnEnd := strings.Index(body[fnStart+len(fnSig):], "\nfunc ")
	require.NotEqual(t, -1, fnEnd)
	fnBody := body[fnStart : fnStart+len(fnSig)+fnEnd]

	// 1. 复用旧 token 必须走 SET XX 守卫(refreshExistingLoginToken),
	//    不能再无条件 SetAndExpire 复活 token key。
	assert.Contains(t, fnBody, "u.refreshExistingLoginToken(",
		"loginWithAuthCode 复用旧 token 必须经 refreshExistingLoginToken(SET XX)校验,避免复活已登出 token")

	// 2. 必须保留 reuseExistingToken 标记,SetAndExpire 只能在 !reuseExistingToken 分支兜底。
	assert.Contains(t, fnBody, "if !reuseExistingToken {",
		"loginWithAuthCode 只能在 !reuseExistingToken 分支无条件写 token 缓存")

	// 3. token 缓存的最终决策必须在调用 IM 之前完成,否则 IM 会拿到回退前的旧 token。
	guardIdx := strings.Index(fnBody, "u.refreshExistingLoginToken(")
	imIdx := strings.Index(fnBody, "u.ctx.UpdateIMToken(")
	require.NotEqual(t, -1, imIdx, "loginWithAuthCode 必须调用 UpdateIMToken")
	assert.Less(t, guardIdx, imIdx,
		"token 复用/回退决策必须在 UpdateIMToken 之前,确保 IM 使用最终 token")
}
