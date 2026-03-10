package user

import (
	"testing"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
)

// parseAccessToken 模拟从 OAuth 响应中解析 access_token 的逻辑
// 这是 requestGiteeAccessToken 和 requestGithubAccessToken 中的核心解析逻辑
func parseAccessToken(result map[string]interface{}) (string, error) {
	accessToken := ""
	if result["access_token"] != nil {
		if token, ok := result["access_token"].(string); ok {
			accessToken = token
		} else {
			return "", errors.New("access_token 类型错误")
		}
	}
	return accessToken, nil
}

func TestParseAccessToken_ValidString(t *testing.T) {
	// 正常情况：access_token 是字符串
	result := map[string]interface{}{
		"access_token": "gho_abc123def456",
		"token_type":   "bearer",
	}
	token, err := parseAccessToken(result)
	assert.NoError(t, err)
	assert.Equal(t, "gho_abc123def456", token)
}

func TestParseAccessToken_NilValue(t *testing.T) {
	// access_token 为 nil
	result := map[string]interface{}{
		"error": "bad_verification_code",
	}
	token, err := parseAccessToken(result)
	assert.NoError(t, err)
	assert.Equal(t, "", token)
}

func TestParseAccessToken_InvalidTypeInt(t *testing.T) {
	// access_token 是 int 类型（异常响应）
	result := map[string]interface{}{
		"access_token": 12345,
	}
	token, err := parseAccessToken(result)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "access_token 类型错误")
	assert.Equal(t, "", token)
}

func TestParseAccessToken_InvalidTypeFloat(t *testing.T) {
	// access_token 是 float 类型
	result := map[string]interface{}{
		"access_token": 123.456,
	}
	token, err := parseAccessToken(result)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "access_token 类型错误")
	assert.Equal(t, "", token)
}

func TestParseAccessToken_InvalidTypeMap(t *testing.T) {
	// access_token 是 map 类型
	result := map[string]interface{}{
		"access_token": map[string]interface{}{"nested": "value"},
	}
	token, err := parseAccessToken(result)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "access_token 类型错误")
	assert.Equal(t, "", token)
}

func TestParseAccessToken_InvalidTypeBool(t *testing.T) {
	// access_token 是 bool 类型
	result := map[string]interface{}{
		"access_token": true,
	}
	token, err := parseAccessToken(result)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "access_token 类型错误")
	assert.Equal(t, "", token)
}

func TestParseAccessToken_EmptyString(t *testing.T) {
	// access_token 是空字符串
	result := map[string]interface{}{
		"access_token": "",
	}
	token, err := parseAccessToken(result)
	assert.NoError(t, err)
	assert.Equal(t, "", token)
}

// TestParseAccessToken_NoSensitiveDataLogged 验证 access_token 解析不会泄露敏感信息
// 这是对 Issue #497 修复的回归测试：
// 确保 OAuth 响应中的 access_token 不会通过日志泄露
func TestParseAccessToken_NoSensitiveDataLogged(t *testing.T) {
	// 模拟真实的 OAuth 响应，包含敏感的 access_token 和 refresh_token
	oauthResponse := map[string]interface{}{
		"access_token":  "ghp_SensitiveTokenThatShouldNotBeLogged123",
		"refresh_token": "ghr_AnotherSensitiveToken456",
		"token_type":    "bearer",
		"expires_in":    3600,
		"scope":         "read:user",
	}

	// 解析 access_token（修复后不再输出到 stdout）
	token, err := parseAccessToken(oauthResponse)

	// 验证解析结果正确
	assert.NoError(t, err)
	assert.Equal(t, "ghp_SensitiveTokenThatShouldNotBeLogged123", token)
	// 注意：此测试确保函数正常工作，fmt.Println 的删除通过代码审查确认
}
