package user

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/stretchr/testify/require"
)

type fakeExistingTokenSetter struct {
	cache *common.MemoryCache
}

func (f fakeExistingTokenSetter) SetIfExists(key string, value string, expire time.Duration) (bool, error) {
	got, err := f.cache.Get(key)
	if err != nil {
		return false, err
	}
	if got == "" {
		return false, nil
	}
	return true, f.cache.SetAndExpire(key, value, expire)
}

// Web/PC 登录复用 uidtoken 里的旧 token 时,必须使用 "SET XX" 语义刷新 token。
// 如果 OIDC logout 已经先删除 token:<oldToken>,登录路径不能把同一个 token key
// 重新创建出来,否则刚 logout 的 HTTP token 会被并发登录复活。
func TestRefreshExistingLoginToken_DoesNotRecreateDeletedToken(t *testing.T) {
	c := common.NewMemoryCache()
	u := &User{existingTokenSetter: fakeExistingTokenSetter{cache: c}}

	ok, err := u.refreshExistingLoginToken("token:logged-out", "payload", time.Minute)
	require.NoError(t, err)
	require.False(t, ok, "missing token key must not be recreated")

	got, err := c.Get("token:logged-out")
	require.NoError(t, err)
	require.Empty(t, got)
}
