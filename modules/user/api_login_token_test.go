package user

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
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

type recordingAPPSessionStore struct {
	oldToken string
	calls    []string
}

func (s *recordingAPPSessionStore) IssueNew(_ context.Context, token, _ string, _ string, deviceFlag int) error {
	s.calls = append(s.calls, "issue:"+token)
	if deviceFlag != int(config.APP) {
		return errors.New("unexpected device flag")
	}
	return nil
}

func (s *recordingAPPSessionStore) DeviceToken(_ context.Context, _ string, deviceFlag int) (string, error) {
	s.calls = append(s.calls, "device_token")
	if deviceFlag != int(config.APP) {
		return "", errors.New("unexpected device flag")
	}
	return s.oldToken, nil
}

func (s *recordingAPPSessionStore) DeleteToken(_ context.Context, token string) error {
	s.calls = append(s.calls, "delete:"+token)
	return nil
}

func (s *recordingAPPSessionStore) ReuseExisting(context.Context, string, string, string, int) (bool, error) {
	return false, nil
}

func (s *recordingAPPSessionStore) UpdatePayloadKeepDeadline(context.Context, string, string) (bool, error) {
	return false, nil
}

func (s *recordingAPPSessionStore) RevokeIssued(context.Context, string, string, int) error {
	return nil
}

type appTokenReplacer interface {
	replaceAPPToken(context.Context, string, string, string) error
}

func TestReplaceAPPTokenRevokesPreviousBearerBeforeIssue(t *testing.T) {
	store := &recordingAPPSessionStore{oldToken: "old-app-token"}
	u := &User{sessionStore: store}
	replacer, ok := interface{}(u).(appTokenReplacer)
	require.True(t, ok, "device verification login must share the APP replacement policy")

	require.NoError(t, replacer.replaceAPPToken(context.Background(), "new-app-token", "payload", "u1"))
	require.Equal(t, []string{
		"device_token",
		"delete:old-app-token",
		"issue:new-app-token",
	}, store.calls)
}

func TestLoginCheckPhoneUsesAPPTokenReplacementBeforeIMUpdate(t *testing.T) {
	fset := gotoken.NewFileSet()
	file, err := parser.ParseFile(fset, "api.go", nil, 0)
	require.NoError(t, err)

	var replacePos, imPos gotoken.Pos
	for _, declaration := range file.Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "loginCheckPhone" {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch selector.Sel.Name {
			case "replaceAPPToken", "replaceAPPTokenSession":
				replacePos = call.Pos()
			case "UpdateIMToken":
				imPos = call.Pos()
			}
			return true
		})
	}

	require.NotZero(t, replacePos, "loginCheckPhone must revoke the previous APP bearer before issuing a new one")
	require.NotZero(t, imPos, "loginCheckPhone must still update the IM credential")
	require.Less(t, replacePos, imPos, "HTTP bearer replacement must finish before the IM token is published")
}
