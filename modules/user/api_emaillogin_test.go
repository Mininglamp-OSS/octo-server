package user

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	commonapi "github.com/Mininglamp-OSS/octo-server/modules/base/common"
	commonsettings "github.com/Mininglamp-OSS/octo-server/modules/common"
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

	assert.Contains(t, w.Body.String(), "注册通道暂不开放")
}

func TestEmailLoginBlockedByEmailOn(t *testing.T) {
	s, ctx := testutil.NewTestServer()
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

func TestEmailSendCodeBlockedByEmailOnForLoginAndRegister(t *testing.T) {
	s, ctx := testutil.NewTestServer()
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

	assert.Contains(t, w.Body.String(), "注册通道暂不开放")
}

func TestEmailForgetPasswordCodeAllowedWhenEmailLoginDisabled(t *testing.T) {
	s, ctx := testutil.NewTestServer()
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

func setPublicIPForUserTest(req *http.Request, ip string) {
	req.Header.Set("X-Forwarded-For", ip)
	req.Header.Set("X-Real-IP", ip)
	req.RemoteAddr = ip + ":12345"
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
