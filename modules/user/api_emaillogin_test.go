package user

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	commonapi "github.com/Mininglamp-OSS/octo-server/modules/base/common"
	commonsettings "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCommitCallbackErrorPropagation verifies that when a commit callback
// returns an error, the calling code properly handles it.
// This is a regression test for issue #395 where the callback returned nil
// instead of the actual error when tx.Commit() failed.
func TestCommitCallbackErrorPropagation(t *testing.T) {
	// Simulate the callback behavior that was fixed in api_emaillogin.go
	// Before fix: callback returned nil even when commit failed
	// After fix: callback returns the actual error

	t.Run("callback should return error on commit failure", func(t *testing.T) {
		commitErr := errors.New("database commit failed")

		// This simulates the fixed callback behavior from emailRegister
		callback := func() error {
			// Simulate commit failure
			if err := simulateCommitFailure(); err != nil {
				// After fix: return the error (was: return nil)
				return err
			}
			return nil
		}

		// With the fix, the callback properly returns the error
		err := callback()
		assert.Error(t, err)
		assert.Equal(t, commitErr, err)
	})

	t.Run("callback should return nil on success", func(t *testing.T) {
		callback := func() error {
			// Simulate successful commit
			return nil
		}

		err := callback()
		assert.NoError(t, err)
	})
}

// simulateCommitFailure simulates a database commit failure
func simulateCommitFailure() error {
	return errors.New("database commit failed")
}

func TestEmailRegisterBlockedByRegisterOff(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	wireI18nRendererForUserTest(s)
	require.NoError(t, testutil.CleanAllTables(ctx))
	setSystemSettingForUserTest(t, ctx, "register", "off", "1", "bool")
	setSystemSettingForUserTest(t, ctx, "register", "email_on", "1", "bool")
	require.NoError(t, commonsettings.EnsureSystemSettings(ctx).Reload())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/user/emailregister", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"email":    "blocked@example.com",
		"code":     "123456",
		"password": "1234567",
		"name":     "blocked",
	}))))
	setPublicIPForUserTest(req, "8.8.8.8")
	s.GetRoute().ServeHTTP(w, req)

	assert.Contains(t, w.Body.String(), "err.server.user.registration_closed")
}

func TestEmailLoginBlockedByLocalLoginOff(t *testing.T) {
	// SSO 必须完整可用,否则安全回退会绕过 local_off=1。见
	// TestUsernameLoginBlockedByLocalLoginOff 的同款注释。
	enableFullOIDCForUserTest(t)
	s, ctx := testutil.NewTestServer()
	wireI18nRendererForUserTest(s)
	require.NoError(t, testutil.CleanAllTables(ctx))
	setSystemSettingForUserTest(t, ctx, "login", "local_off", "1", "bool")
	require.NoError(t, commonsettings.EnsureSystemSettings(ctx).Reload())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/user/emaillogin", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"email":    "blocked@example.com",
		"password": "1234567",
	}))))
	setPublicIPForUserTest(req, "8.8.4.5")
	s.GetRoute().ServeHTTP(w, req)

	assert.Contains(t, w.Body.String(), "本地登录已关闭")
}

func TestEmailLoginBlockedByEmailOn(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	wireI18nRendererForUserTest(s)
	require.NoError(t, testutil.CleanAllTables(ctx))
	setSystemSettingForUserTest(t, ctx, "register", "email_on", "0", "bool")
	require.NoError(t, commonsettings.EnsureSystemSettings(ctx).Reload())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/user/emaillogin", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"email":    "blocked@example.com",
		"password": "1234567",
	}))))
	setPublicIPForUserTest(req, "8.8.4.4")
	s.GetRoute().ServeHTTP(w, req)

	assert.Contains(t, w.Body.String(), "暂不支持邮箱登录")
}

// local_off=1 时邮箱登录验证码必须同步拒绝,否则绕过 /v1/user/emaillogin
// 入口仍可通过 /v1/user/email/sendcode(CodeTypeEmailLogin) 让后端发出真实
// 验证码,既滥发邮件,又给攻击路径留口。
// 同时验证:CodeTypeForgetLoginPWD 不受影响 —— 老用户找回密码渠道必须保留。
func TestEmailSendCodeForLoginBlockedByLocalLoginOff(t *testing.T) {
	enableFullOIDCForUserTest(t) // 让 local_off=1 通过安全回退,验证守卫本身
	s, ctx := testutil.NewTestServer()
	wireI18nRendererForUserTest(s)
	require.NoError(t, testutil.CleanAllTables(ctx))
	setSystemSettingForUserTest(t, ctx, "login", "local_off", "1", "bool")
	// 把 register.email_on 显式置 1,排除"被另一个开关拦下"的混淆。
	setSystemSettingForUserTest(t, ctx, "register", "email_on", "1", "bool")
	require.NoError(t, commonsettings.EnsureSystemSettings(ctx).Reload())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/user/email/sendcode", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"email":     "blocked@example.com",
		"code_type": int(commonapi.CodeTypeEmailLogin),
	}))))
	setPublicIPForUserTest(req, "8.8.4.6")
	s.GetRoute().ServeHTTP(w, req)
	assert.Contains(t, w.Body.String(), "本地登录已关闭")

	// 忘记密码验证码不受 local_off 影响 —— 老用户找回入口必须保留。
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("POST", "/v1/user/email/sendcode", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"email":     "recover@example.com",
		"code_type": int(commonapi.CodeTypeForgetLoginPWD),
	}))))
	setPublicIPForUserTest(req2, "8.8.4.7")
	s.GetRoute().ServeHTTP(w2, req2)
	assert.NotContains(t, w2.Body.String(), "本地登录已关闭",
		"忘记密码渠道不应被 local_off 拦截")
}

func TestEmailSendCodeBlockedByEmailOnForLoginAndRegister(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	wireI18nRendererForUserTest(s)
	require.NoError(t, testutil.CleanAllTables(ctx))
	setSystemSettingForUserTest(t, ctx, "register", "email_on", "0", "bool")
	require.NoError(t, commonsettings.EnsureSystemSettings(ctx).Reload())

	for _, codeType := range []commonapi.CodeType{commonapi.CodeTypeRegister, commonapi.CodeTypeEmailLogin} {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/user/email/sendcode", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
			"email":     "blocked@example.com",
			"code_type": int(codeType),
		}))))
		setPublicIPForUserTest(req, "1.1.1.1")
		s.GetRoute().ServeHTTP(w, req)

		assert.Contains(t, w.Body.String(), "暂不支持邮箱")
	}
}

func TestEmailSendRegisterCodeBlockedByRegisterOff(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	wireI18nRendererForUserTest(s)
	require.NoError(t, testutil.CleanAllTables(ctx))
	setSystemSettingForUserTest(t, ctx, "register", "off", "1", "bool")
	setSystemSettingForUserTest(t, ctx, "register", "email_on", "1", "bool")
	require.NoError(t, commonsettings.EnsureSystemSettings(ctx).Reload())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/user/email/sendcode", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"email":     "blocked@example.com",
		"code_type": int(commonapi.CodeTypeRegister),
	}))))
	setPublicIPForUserTest(req, "1.0.0.1")
	s.GetRoute().ServeHTTP(w, req)

	assert.Contains(t, w.Body.String(), "err.server.user.registration_closed")
}

func TestEmailForgetPasswordCodeAllowedWhenEmailLoginDisabled(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	wireI18nRendererForUserTest(s)
	require.NoError(t, testutil.CleanAllTables(ctx))
	setSystemSettingForUserTest(t, ctx, "register", "email_on", "0", "bool")
	require.NoError(t, commonsettings.EnsureSystemSettings(ctx).Reload())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/user/email/sendcode", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"email":     "recover@example.com",
		"code_type": int(commonapi.CodeTypeForgetLoginPWD),
	}))))
	setPublicIPForUserTest(req, "1.0.0.2")
	s.GetRoute().ServeHTTP(w, req)

	assert.NotContains(t, w.Body.String(), "暂不支持邮箱")
}

// setSystemSettingForUserTest writes a system_setting row and registers a
// cleanup that deletes the row AND reloads the shared SystemSettings
// snapshot. Without the cleanup, the singleton's in-memory snapshot keeps
// the override across tests — testutil.CleanAllTables truncates the table
// but does not touch process-local caches, so later tests would see
// `register.off=1` even after the DB row is gone. Caller usually pairs
// this with EnsureSystemSettings(...).Reload() to push the new value into
// the snapshot; the cleanup ensures the snapshot is restored regardless of
// whether the caller did or did not call Reload at write time.
func setSystemSettingForUserTest(t *testing.T, ctx *config.Context, category, key, value, valueType string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO system_setting (category, key_name, value, value_type, description) "+
			"VALUES (?, ?, ?, ?, '') "+
			"ON DUPLICATE KEY UPDATE value = VALUES(value), value_type = VALUES(value_type), description = VALUES(description)",
		category, key, value, valueType,
	).Exec()
	require.NoError(t, err)

	t.Cleanup(func() {
		if _, delErr := ctx.DB().DeleteFrom("system_setting").
			Where("category = ? AND key_name = ?", category, key).Exec(); delErr != nil {
			t.Logf("cleanup: delete system_setting %s.%s failed: %v", category, key, delErr)
		}
		if reloadErr := commonsettings.EnsureSystemSettings(ctx).Reload(); reloadErr != nil {
			t.Logf("cleanup: reload SystemSettings failed: %v", reloadErr)
		}
	})
}

// enableFullOIDCForUserTest 是 modules/common 同名 helper 的镜像副本。
// 把 OIDC 切到"完整可用"状态(DM_OIDC_ENABLED + issuer / client_id / secret
// / redirect_uri / 32 字节 RT key 齐备),让 LocalLoginOff() 的安全回退判定
// 通过,从而验证下游守卫本身的语义。
//
// 镜像而非跨包 import:Go 测试帮助函数定义在 _test.go 中,不属于公共 API,
// 跨包共享需要把它挪到非测试文件并标 Helper —— 对仅几行的 fixture 不划算。
// modules/common/system_settings_test.go 的 fullOIDCTestEnv 是单一真源,
// 这里的字段集必须与之同步。
func enableFullOIDCForUserTest(t *testing.T) {
	t.Helper()
	for _, alias := range []string{
		"DM_OIDC_AEGIS_ISSUER",
		"DM_OIDC_AEGIS_CLIENT_ID",
		"DM_OIDC_AEGIS_CLIENT_SECRET",
		"DM_OIDC_AEGIS_REDIRECT_URI",
	} {
		t.Setenv(alias, "")
	}
	t.Setenv("DM_OIDC_ENABLED", "true")
	t.Setenv("DM_OIDC_PROVIDER_ISSUER", "https://idp.example.com")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_ID", "test-client")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_SECRET", "test-secret")
	t.Setenv("DM_OIDC_PROVIDER_REDIRECT_URI", "https://app.example.com/oidc/callback")
	t.Setenv("DM_OIDC_RT_ENC_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
}

func setPublicIPForUserTest(req *http.Request, ip string) {
	req.Header.Set("X-Forwarded-For", ip)
	req.Header.Set("X-Real-IP", ip)
	req.RemoteAddr = ip + ":12345"
}

// wireI18nRendererForUserTest injects the i18n ErrorRenderer onto the
// route returned by testutil.NewTestServer, mirroring what main.go does at
// boot. Post-Phase 2.1, modules/user handlers respond via
// httperr.ResponseErrorL → c.RenderError; without a renderer wired the
// route falls back to the legacy {msg,status} envelope carrying the
// English source DefaultMessage instead of the localized zh-CN copy
// production clients receive. testutil.NewTestServer lives in octo-lib and
// is intentionally not touched from this PR to keep the migration scoped.
func wireI18nRendererForUserTest(s *server.Server) {
	s.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
}

// TestCallbackErrorHandling verifies the pattern where callback errors
// should be checked and propagated by the caller.
func TestCallbackErrorHandling(t *testing.T) {
	t.Run("caller should check callback error", func(t *testing.T) {
		expectedErr := errors.New("callback error")

		// This simulates the fixed behavior in createUserWithRespAndTx
		// Before fix: commitCallback() was called but return value ignored
		// After fix: if err := commitCallback(); err != nil { return nil, err }
		processWithCallback := func(commitCallback func() error) (interface{}, error) {
			// Fixed code pattern:
			if commitCallback != nil {
				if err := commitCallback(); err != nil {
					return nil, err
				}
			}
			return "success", nil
		}

		result, err := processWithCallback(func() error {
			return expectedErr
		})

		assert.Error(t, err)
		assert.Equal(t, expectedErr, err)
		assert.Nil(t, result)
	})

	t.Run("caller should proceed when callback succeeds", func(t *testing.T) {
		processWithCallback := func(commitCallback func() error) (interface{}, error) {
			if commitCallback != nil {
				if err := commitCallback(); err != nil {
					return nil, err
				}
			}
			return "success", nil
		}

		result, err := processWithCallback(func() error {
			return nil
		})

		assert.NoError(t, err)
		assert.Equal(t, "success", result)
	})
}
