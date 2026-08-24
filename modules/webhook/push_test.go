package webhook

import (
	"encoding/json"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOPPOAPIEndpointsUseDocumentedDomesticHost(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		path     string
	}{
		{name: "auth", endpoint: oppoAuthURL, path: "/server/v1/auth"},
		{name: "notification unicast", endpoint: oppoNotificationUnicastURL, path: "/server/v1/message/notification/unicast"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.endpoint)
			assert.NoError(t, err)
			assert.Equal(t, "https", u.Scheme)
			assert.Equal(t, "api-push-cn.heytapmobi.com", u.Host)
			assert.Equal(t, tt.path, u.Path)
		})
	}
}

func TestHMSPush(t *testing.T) {
	appID := os.Getenv("HMS_APP_ID")
	appSecret := os.Getenv("HMS_APP_SECRET")
	packageName := os.Getenv("HMS_PACKAGE_NAME")
	deviceToken := os.Getenv("HMS_DEVICE_TOKEN")
	if appID == "" || appSecret == "" || packageName == "" || deviceToken == "" {
		t.Skip("HMS push credentials not configured (set HMS_APP_ID, HMS_APP_SECRET, HMS_PACKAGE_NAME, HMS_DEVICE_TOKEN)")
	}
	hms := NewHMSPush(appID, appSecret, packageName)
	accessToken, _, err := hms.GetHMSAccessToken()
	assert.NoError(t, err)
	payloadInfo := &PayloadInfo{
		Title:   "title",
		Content: "content2222",
		Badge:   1,
	}
	err = hms.Push(deviceToken, NewHMSPayload(payloadInfo, accessToken))
	assert.NoError(t, err)
}

func TestMIPush(t *testing.T) {
	appID := os.Getenv("MI_APP_ID")
	appSecret := os.Getenv("MI_APP_SECRET")
	packageName := os.Getenv("MI_PACKAGE_NAME")
	channelID := os.Getenv("MI_CHANNEL_ID")
	deviceToken := os.Getenv("MI_DEVICE_TOKEN")
	if appID == "" || appSecret == "" || packageName == "" || deviceToken == "" {
		t.Skip("MI push credentials not configured (set MI_APP_ID, MI_APP_SECRET, MI_PACKAGE_NAME, MI_DEVICE_TOKEN)")
	}
	mi := NewMIPush(appID, appSecret, packageName, channelID)

	payloadInfo := &PayloadInfo{
		Title:   "title",
		Content: "content",
		Badge:   1,
	}

	err := mi.Push(deviceToken, NewMIPayload(payloadInfo, "11"))
	assert.NoError(t, err)
}

func TestOPPOPush(t *testing.T) {
	appKey := os.Getenv("OPPO_APP_KEY")
	masterSecret := os.Getenv("OPPO_MASTER_SECRET")
	deviceToken := os.Getenv("OPPO_DEVICE_TOKEN")
	if appKey == "" || masterSecret == "" || deviceToken == "" {
		t.Skip("OPPO push credentials not configured (set OPPO_APP_KEY, OPPO_MASTER_SECRET, OPPO_DEVICE_TOKEN)")
	}
	oppo := NewOPPOPush("", appKey, "", masterSecret, nil)
	payloadInfo := &PayloadInfo{
		Title:   "标题",
		Content: "内容",
		Badge:   1,
	}
	err := oppo.Push(deviceToken, NewOPPOPayload(payloadInfo, "11"))
	assert.NoError(t, err)
}

func TestOPPOAuth(t *testing.T) {
	appKey := os.Getenv("OPPO_APP_KEY")
	masterSecret := os.Getenv("OPPO_MASTER_SECRET")
	if appKey == "" || masterSecret == "" {
		t.Skip("OPPO credentials not configured (set OPPO_APP_KEY and OPPO_MASTER_SECRET)")
	}

	oppo := NewOPPOPush("", appKey, "", masterSecret, nil)
	require.NoError(t, oppo.configErr)
	token, err := oppo.getAuthToken()
	require.NoError(t, err)
	require.NotEmpty(t, token)
}

func TestVIVOPush(t *testing.T) {
	appID := os.Getenv("VIVO_APP_ID")
	appKey := os.Getenv("VIVO_APP_KEY")
	appSecret := os.Getenv("VIVO_APP_SECRET")
	deviceToken := os.Getenv("VIVO_DEVICE_TOKEN")
	if appID == "" || appKey == "" || appSecret == "" || deviceToken == "" {
		t.Skip("VIVO push credentials not configured (set VIVO_APP_ID, VIVO_APP_KEY, VIVO_APP_SECRET, VIVO_DEVICE_TOKEN)")
	}
	vivo := NewVIVOPush(appID, appKey, appSecret, &config.Context{})
	payloadInfo := &PayloadInfo{
		Title:   "标题",
		Content: "内容",
		Badge:   1,
	}
	err := vivo.Push(deviceToken, NewVIVOPayload(payloadInfo, "11"))
	assert.NoError(t, err)
}

func TestFirebasePush(t *testing.T) {
	jsonPath := os.Getenv("FIREBASE_JSON_PATH")
	packageName := os.Getenv("FIREBASE_PACKAGE_NAME")
	projectID := os.Getenv("FIREBASE_PROJECT_ID")
	deviceToken := os.Getenv("FIREBASE_DEVICE_TOKEN")
	if jsonPath == "" || packageName == "" || projectID == "" || deviceToken == "" {
		t.Skip("Firebase push credentials not configured (set FIREBASE_JSON_PATH, FIREBASE_PACKAGE_NAME, FIREBASE_PROJECT_ID, FIREBASE_DEVICE_TOKEN)")
	}
	firebase := NewFIREBASEPush(jsonPath, packageName, projectID, "")

	payloadInfo := &PayloadInfo{
		Title:   "title",
		Content: "content",
		Badge:   1,
	}
	err := firebase.Push(deviceToken, NewFIREBASEPayload(payloadInfo, "11"))
	assert.NoError(t, err)
}

func TestParseOPPOAuthResponse(t *testing.T) {
	tests := []struct {
		name        string
		resp        map[string]interface{}
		wantToken   string
		wantErr     bool
		errContains string
	}{
		{
			name:        "nil response",
			resp:        nil,
			wantErr:     true,
			errContains: "empty response",
		},
		{
			name:        "missing code",
			resp:        map[string]interface{}{},
			wantErr:     true,
			errContains: "empty response",
		},
		{
			name: "code wrong type",
			resp: map[string]interface{}{
				"code": "not_a_number",
			},
			wantErr:     true,
			errContains: "unexpected code type",
		},
		{
			name: "auth failed with error code",
			resp: map[string]interface{}{
				"code":    json.Number("1001"),
				"message": "invalid credentials",
			},
			wantErr:     true,
			errContains: "auth failed",
		},
		{
			name: "data wrong type",
			resp: map[string]interface{}{
				"code": json.Number("0"),
				"data": "not_a_map",
			},
			wantErr:     true,
			errContains: "unexpected data type",
		},
		{
			name: "auth_token wrong type",
			resp: map[string]interface{}{
				"code": json.Number("0"),
				"data": map[string]interface{}{
					"auth_token": 12345,
				},
			},
			wantErr:     true,
			errContains: "unexpected auth_token type",
		},
		{
			name: "valid response",
			resp: map[string]interface{}{
				"code": json.Number("0"),
				"data": map[string]interface{}{
					"auth_token": "oppo_token_abc123",
				},
			},
			wantToken: "oppo_token_abc123",
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, err := parseOPPOAuthResponse(tt.resp)
			if tt.wantErr {
				assert.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.wantToken, token)
			}
		})
	}
}

func TestMIPushResponseHandling(t *testing.T) {
	tests := []struct {
		name      string
		result    map[string]interface{}
		wantErr   bool
		errString string
	}{
		{
			name:    "nil result",
			result:  nil,
			wantErr: false,
		},
		{
			name: "result ok",
			result: map[string]interface{}{
				"result": "ok",
			},
			wantErr: false,
		},
		{
			name: "result not ok with reason",
			result: map[string]interface{}{
				"result": "error",
				"reason": "invalid token",
			},
			wantErr:   true,
			errString: "invalid token",
		},
		{
			name: "result not ok with description",
			result: map[string]interface{}{
				"result":      "error",
				"description": "server error",
			},
			wantErr:   true,
			errString: "server error",
		},
		{
			name: "result not ok without reason or description",
			result: map[string]interface{}{
				"result": "error",
			},
			wantErr:   true,
			errString: "MI push failed with unknown error",
		},
		{
			name: "result field missing (not string type)",
			result: map[string]interface{}{
				"result": 123,
			},
			wantErr: false,
		},
		{
			name: "reason wrong type should not panic",
			result: map[string]interface{}{
				"result": "error",
				"reason": 123,
			},
			wantErr:   true,
			errString: "MI push failed with unknown error",
		},
		{
			name: "description wrong type should not panic",
			result: map[string]interface{}{
				"result":      "error",
				"description": 456,
			},
			wantErr:   true,
			errString: "MI push failed with unknown error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkMIPushResult(tt.result)
			if tt.wantErr {
				assert.Error(t, err)
				if tt.errString != "" {
					assert.Equal(t, tt.errString, err.Error())
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestParseHMSAuthResponse(t *testing.T) {
	tests := []struct {
		name        string
		resultMap   map[string]interface{}
		wantToken   string
		wantExpire  time.Duration
		wantErr     bool
		errContains string
	}{
		{
			name:        "nil response",
			resultMap:   nil,
			wantErr:     true,
			errContains: "empty response",
		},
		{
			name:        "missing access_token",
			resultMap:   map[string]interface{}{},
			wantErr:     true,
			errContains: "unexpected access_token type",
		},
		{
			name: "access_token wrong type (int)",
			resultMap: map[string]interface{}{
				"access_token": 12345,
			},
			wantErr:     true,
			errContains: "unexpected access_token type",
		},
		{
			name: "valid response with expires_in",
			resultMap: map[string]interface{}{
				"access_token": "test_token_abc",
				"expires_in":   json.Number("7200"),
			},
			wantToken:  "test_token_abc",
			wantExpire: 7200 * time.Second,
			wantErr:    false,
		},
		{
			name: "valid response without expires_in (default 1h)",
			resultMap: map[string]interface{}{
				"access_token": "test_token_xyz",
			},
			wantToken:  "test_token_xyz",
			wantExpire: 3600 * time.Second,
			wantErr:    false,
		},
		{
			name: "expires_in wrong type (string) falls back to default",
			resultMap: map[string]interface{}{
				"access_token": "test_token_fallback",
				"expires_in":   "not_a_number",
			},
			wantToken:  "test_token_fallback",
			wantExpire: 3600 * time.Second,
			wantErr:    false,
		},
		{
			name: "expires_in zero falls back to default",
			resultMap: map[string]interface{}{
				"access_token": "test_token_zero",
				"expires_in":   json.Number("0"),
			},
			wantToken:  "test_token_zero",
			wantExpire: 3600 * time.Second,
			wantErr:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, expire, err := parseHMSAuthResponse(tt.resultMap)
			if tt.wantErr {
				assert.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.wantToken, token)
				assert.Equal(t, tt.wantExpire, expire)
			}
		})
	}
}
