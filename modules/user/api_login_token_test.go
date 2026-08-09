package user

import (
	"context"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/stretchr/testify/require"
)

type fakeUserSessionStore struct {
	cache *common.MemoryCache
}

func (f fakeUserSessionStore) ReuseExisting(_ context.Context, token, value, _ string, _ int) (bool, error) {
	key := "token:" + token
	got, err := f.cache.Get(key)
	if err != nil {
		return false, err
	}
	if got == "" {
		return false, nil
	}
	return true, f.cache.SetAndExpire(key, value, time.Minute)
}

func (f fakeUserSessionStore) IssueNew(context.Context, string, string, string, int) error {
	return nil
}
func (f fakeUserSessionStore) UpdatePayloadKeepDeadline(context.Context, string, string) (bool, error) {
	return false, nil
}
func (f fakeUserSessionStore) DeviceToken(context.Context, string, int) (string, error) {
	return "", nil
}
func (f fakeUserSessionStore) DeleteToken(context.Context, string) error { return nil }
func (f fakeUserSessionStore) RevokeIssued(context.Context, string, string, int) error {
	return nil
}

// Web/PC 登录复用 uidtoken 里的旧 token 时,必须使用 "SET XX" 语义刷新 token。
// 如果 OIDC logout 已经先删除 token:<oldToken>,登录路径不能把同一个 token key
// 重新创建出来,否则刚 logout 的 HTTP token 会被并发登录复活。
func TestRefreshExistingLoginToken_DoesNotRecreateDeletedToken(t *testing.T) {
	c := common.NewMemoryCache()
	u := &User{sessionStore: fakeUserSessionStore{cache: c}}

	ok, err := u.reuseExistingLoginToken(context.Background(), "logged-out", "payload", "u1", 1)
	require.NoError(t, err)
	require.False(t, ok, "missing token key must not be recreated")

	got, err := c.Get("token:logged-out")
	require.NoError(t, err)
	require.Empty(t, got)
}
