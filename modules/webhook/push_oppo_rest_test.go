package webhook

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var oppoTestNow = time.UnixMilli(1_700_000_000_123)

type memoryOPPOTokenCache struct {
	mu        sync.Mutex
	token     string
	getErr    error
	setErr    error
	delErr    error
	getKeys   []string
	setKeys   []string
	delKeys   []string
	setTTLs   []time.Duration
	setValues []string
}

type oppoDoerFunc func(*http.Request) (*http.Response, error)

type recordedOPPOLogEntry struct {
	message string
	fields  map[string]interface{}
}

type recordingOPPOLogger struct {
	mu       sync.Mutex
	warnings []recordedOPPOLogEntry
	debugs   []recordedOPPOLogEntry
}

func (f oppoDoerFunc) Do(request *http.Request) (*http.Response, error) {
	return f(request)
}

func (l *recordingOPPOLogger) Info(string, ...zap.Field)  {}
func (l *recordingOPPOLogger) Error(string, ...zap.Field) {}

func (l *recordingOPPOLogger) Debug(message string, fields ...zap.Field) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.debugs = append(l.debugs, recordedOPPOLogEntry{message: message, fields: encodeOPPOLogFields(fields)})
}

func (l *recordingOPPOLogger) Warn(message string, fields ...zap.Field) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warnings = append(l.warnings, recordedOPPOLogEntry{message: message, fields: encodeOPPOLogFields(fields)})
}

func encodeOPPOLogFields(fields []zap.Field) map[string]interface{} {
	encoder := zapcore.NewMapObjectEncoder()
	for _, field := range fields {
		field.AddTo(encoder)
	}
	return encoder.Fields
}

func cachedOPPOAuthToken(t *testing.T, token string, expiresAt time.Time) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]interface{}{
		"auth_token": token,
		"expires_at": expiresAt.UnixMilli(),
	})
	require.NoError(t, err)
	return string(encoded)
}

func (c *memoryOPPOTokenCache) getCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.getKeys)
}

func (c *memoryOPPOTokenCache) clearToken() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = ""
}

func (l *recordingOPPOLogger) snapshot() (warnings, debugs []recordedOPPOLogEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]recordedOPPOLogEntry(nil), l.warnings...), append([]recordedOPPOLogEntry(nil), l.debugs...)
}

func (c *memoryOPPOTokenCache) GetString(key string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.getKeys = append(c.getKeys, key)
	return c.token, c.getErr
}

func (c *memoryOPPOTokenCache) SetAndExpire(key string, value interface{}, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setKeys = append(c.setKeys, key)
	c.setTTLs = append(c.setTTLs, ttl)
	c.setValues = append(c.setValues, fmt.Sprint(value))
	if c.setErr == nil {
		c.token = fmt.Sprint(value)
	}
	return c.setErr
}

func (c *memoryOPPOTokenCache) Del(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.delKeys = append(c.delKeys, key)
	if c.delErr == nil {
		c.token = ""
	}
	return c.delErr
}

func newOPPOPushForTest(t *testing.T, handler http.Handler, cache *memoryOPPOTokenCache, options oppoPushOptions) *OPPOPush {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	pusher := NewOPPOPush("app-id", "app-key", "app-secret", "master-secret", nil)
	pusher.httpClient = server.Client()
	pusher.authURL = server.URL + "/server/v1/auth"
	pusher.notificationUnicastURL = server.URL + "/server/v1/message/notification/unicast"
	pusher.tokenCache = cache
	pusher.options = options
	pusher.configErr = nil
	pusher.now = func() time.Time { return oppoTestNow }
	return pusher
}

func writeOPPOJSON(t *testing.T, w http.ResponseWriter, status int, body string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, err := w.Write([]byte(body))
	require.NoError(t, err)
}

func decodeOPPOMessage(t *testing.T, r *http.Request) map[string]interface{} {
	t.Helper()

	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	require.NoError(t, err)
	require.Equal(t, "application/x-www-form-urlencoded", mediaType)
	require.NoError(t, r.ParseForm())
	require.Len(t, r.PostForm, 2)

	var message map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(r.PostForm.Get("message")), &message))
	return message
}

func TestOPPOPushUsesEncodedFormAndCurrentNotificationPayload(t *testing.T) {
	cache := &memoryOPPOTokenCache{token: cachedOPPOAuthToken(t, "cached&token+%", oppoTestNow.Add(oppoAuthTokenTTL))}
	var gotMessage map[string]interface{}
	var requestCount int
	pusher := newOPPOPushForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		require.Equal(t, "/server/v1/message/notification/unicast", r.URL.Path)
		gotMessage = decodeOPPOMessage(t, r)
		require.Equal(t, "cached&token+%", r.PostForm.Get("auth_token"))
		writeOPPOJSON(t, w, http.StatusOK, `{"code":0,"message":"","data":{"messageId":"oppo-message-id"}}`)
	}), cache, oppoPushOptions{
		category:                 "IM",
		notifyLevel:              2,
		channelID:                "messages",
		privateMessageTemplateID: "private-template-1",
		clickActionType:          4,
		clickActionActivity:      "com.example.chat.MainActivity",
		offlineTTLSeconds:        86400,
	})

	payload := NewOPPOPayload(&PayloadInfo{
		Title:       "标题 & + %",
		Content:     "内容=甲&乙+丙%",
		Badge:       3,
		SpaceID:     "space-A",
		ChannelID:   "group&+%",
		ChannelType: 2,
		MessageSeq:  42,
	}, "42")

	require.NoError(t, pusher.Push("CN_token&+%", payload))
	require.Equal(t, 1, requestCount)
	require.Equal(t, float64(2), gotMessage["target_type"])
	require.Equal(t, "CN_token&+%", gotMessage["target_value"])
	require.Equal(t, true, gotMessage["verify_registration_id"])

	notification, ok := gotMessage["notification"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "标题 & + %", notification["title"])
	assert.Equal(t, "内容=甲&乙+丙%", notification["content"])
	assert.Equal(t, "IM", notification["category"])
	assert.Equal(t, float64(2), notification["notify_level"])
	assert.Equal(t, "messages", notification["channel_id"])
	assert.Equal(t, "private-template-1", notification["private_msg_template_id"])
	assert.Equal(t, float64(4), notification["click_action_type"])
	assert.Equal(t, "com.example.chat.MainActivity", notification["click_action_activity"])
	assert.Equal(t, true, notification["off_line"])
	assert.Equal(t, float64(86400), notification["off_line_ttl"])
	assert.NotContains(t, notification, "notify_id")
	assert.NotEmpty(t, notification["app_message_id"])

	var actionParameters map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(notification["action_parameters"].(string)), &actionParameters))
	assert.Equal(t, "space-A", actionParameters["space_id"])
	assert.Equal(t, "group&+%", actionParameters["channel_id"])
	assert.Equal(t, float64(2), actionParameters["channel_type"])
	assert.Equal(t, float64(42), actionParameters["message_seq"])
}

func TestOPPOPushAuthenticatesWithDocumentedSignatureAndCachesToken(t *testing.T) {
	cache := &memoryOPPOTokenCache{}
	authCalls := 0
	unicastCalls := 0
	pusher := newOPPOPushForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/server/v1/auth":
			authCalls++
			require.NoError(t, r.ParseForm())
			require.Equal(t, "app-key", r.PostForm.Get("app_key"))
			require.Equal(t, "1700000000123", r.PostForm.Get("timestamp"))
			digest := sha256.Sum256([]byte("app-key1700000000123master-secret"))
			require.Equal(t, hex.EncodeToString(digest[:]), r.PostForm.Get("sign"))
			writeOPPOJSON(t, w, http.StatusOK, `{"code":0,"message":"success","data":{"auth_token":"fresh-token"}}`)
		case "/server/v1/message/notification/unicast":
			unicastCalls++
			require.NoError(t, r.ParseForm())
			require.Equal(t, "fresh-token", r.PostForm.Get("auth_token"))
			writeOPPOJSON(t, w, http.StatusOK, `{"code":0}`)
		default:
			http.NotFound(w, r)
		}
	}), cache, defaultOPPOPushOptions())

	payload := NewOPPOPayload(&PayloadInfo{Title: "title", Content: "content", MessageSeq: 7}, "7")
	require.NoError(t, pusher.Push("registration-id", payload))
	require.Equal(t, 1, authCalls)
	require.Equal(t, 1, unicastCalls)
	require.Len(t, cache.setValues, 1)
	var cachedToken struct {
		AuthToken string `json:"auth_token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	require.NoError(t, json.Unmarshal([]byte(cache.setValues[0]), &cachedToken))
	assert.Equal(t, "fresh-token", cachedToken.AuthToken)
	assert.Equal(t, oppoTestNow.Add(oppoAuthTokenTTL).UnixMilli(), cachedToken.ExpiresAt)
	require.Equal(t, []time.Duration{20 * time.Hour}, cache.setTTLs)
	require.Len(t, cache.setKeys, 1)
	assert.Contains(t, cache.setKeys[0], "oppo_auth_token:")
	assert.NotContains(t, cache.setKeys[0], "master-secret")

	require.NoError(t, pusher.Push("registration-id", payload))
	require.Equal(t, 1, authCalls, "second push must reuse the cached token")
	require.Equal(t, 2, unicastCalls)
}

func TestOPPOPushUsesProcessLocalTokenOnConcurrentNormalPath(t *testing.T) {
	cache := &memoryOPPOTokenCache{}
	pusher := newOPPOPushForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/server/v1/auth":
			writeOPPOJSON(t, w, http.StatusOK, `{"code":0,"data":{"auth_token":"fresh-token"}}`)
		case "/server/v1/message/notification/unicast":
			writeOPPOJSON(t, w, http.StatusOK, `{"code":0}`)
		default:
			http.NotFound(w, r)
		}
	}), cache, defaultOPPOPushOptions())
	payload := NewOPPOPayload(&PayloadInfo{Title: "title", Content: "content", MessageSeq: 7}, "7")

	require.NoError(t, pusher.Push("registration-id", payload))
	require.Equal(t, 1, cache.getCount())

	const pushes = 16
	errs := make(chan error, pushes)
	var wg sync.WaitGroup
	for range pushes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- pusher.Push("registration-id", payload)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, 1, cache.getCount(), "a valid process-local token must avoid Redis and the refresh mutex")
}

func TestOPPOPushConcurrentCacheMissAuthenticatesOnce(t *testing.T) {
	cache := &memoryOPPOTokenCache{}
	var mu sync.Mutex
	authCalls := 0
	unicastCalls := 0
	pusher := newOPPOPushForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/server/v1/auth":
			authCalls++
			writeOPPOJSON(t, w, http.StatusOK, `{"code":0,"data":{"auth_token":"fresh-token"}}`)
		case "/server/v1/message/notification/unicast":
			unicastCalls++
			writeOPPOJSON(t, w, http.StatusOK, `{"code":0}`)
		default:
			http.NotFound(w, r)
		}
	}), cache, defaultOPPOPushOptions())
	payload := NewOPPOPayload(&PayloadInfo{Title: "title", Content: "content", MessageSeq: 7}, "7")

	const pushes = 16
	errs := make(chan error, pushes)
	var wg sync.WaitGroup
	for range pushes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- pusher.Push("registration-id", payload)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, authCalls)
	assert.Equal(t, pushes, unicastCalls)
}

func TestOPPOPushRefreshesExpiredProcessLocalToken(t *testing.T) {
	cache := &memoryOPPOTokenCache{}
	now := oppoTestNow
	authCalls := 0
	var sentTokens []string
	pusher := newOPPOPushForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/server/v1/auth":
			authCalls++
			writeOPPOJSON(t, w, http.StatusOK, fmt.Sprintf(`{"code":0,"data":{"auth_token":"token-%d"}}`, authCalls))
		case "/server/v1/message/notification/unicast":
			require.NoError(t, r.ParseForm())
			sentTokens = append(sentTokens, r.PostForm.Get("auth_token"))
			writeOPPOJSON(t, w, http.StatusOK, `{"code":0}`)
		default:
			http.NotFound(w, r)
		}
	}), cache, defaultOPPOPushOptions())
	pusher.now = func() time.Time { return now }
	payload := NewOPPOPayload(&PayloadInfo{Title: "title", Content: "content", MessageSeq: 7}, "7")

	require.NoError(t, pusher.Push("registration-id", payload))
	now = now.Add(oppoAuthTokenTTL)
	cache.clearToken() // Simulate the Redis TTL expiring at the same instant.
	require.NoError(t, pusher.Push("registration-id", payload))

	assert.Equal(t, 2, authCalls)
	assert.Equal(t, []string{"token-1", "token-2"}, sentTokens)
}

func TestOPPOPushSanitizesRequiredNotificationText(t *testing.T) {
	pusher := NewOPPOPush("app-id", "app-key", "app-secret", "master-secret", nil)
	pusher.configErr = nil

	tests := []struct {
		name        string
		title       string
		content     string
		wantTitle   string
		wantContent string
	}{
		{
			name:        "blank values use safe fallback",
			title:       " \t\n",
			content:     "",
			wantTitle:   "您有一条新的消息",
			wantContent: "您有一条新的消息",
		},
		{
			name:        "multibyte values are truncated by rune",
			title:       strings.Repeat("题", 51),
			content:     strings.Repeat("文", 201),
			wantTitle:   strings.Repeat("题", 50),
			wantContent: strings.Repeat("文", 200),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := pusher.buildUnicastMessage("registration-id", NewOPPOPayload(&PayloadInfo{
				Title:      tt.title,
				Content:    tt.content,
				MessageSeq: 1,
			}, "1"))
			require.NoError(t, err)

			var message oppoUnicastMessage
			require.NoError(t, json.Unmarshal(encoded, &message))
			assert.Equal(t, tt.wantTitle, message.Notification.Title)
			assert.Equal(t, tt.wantContent, message.Notification.Content)
			assert.LessOrEqual(t, len([]rune(message.Notification.Title)), 50)
			assert.LessOrEqual(t, len([]rune(message.Notification.Content)), 200)
		})
	}
}

func TestOPPOPushRejectsOversizedActionParameters(t *testing.T) {
	pusher := NewOPPOPush("app-id", "app-key", "app-secret", "master-secret", nil)
	pusher.configErr = nil
	payload := NewOPPOPayload(&PayloadInfo{
		Title:      "title",
		Content:    "content",
		ChannelID:  strings.Repeat("界", 1400),
		MessageSeq: 1,
	}, "1")

	_, err := pusher.buildUnicastMessage("registration-id", payload)
	require.ErrorContains(t, err, "action parameters exceed 4096 bytes")
}

func TestOPPOPushRefreshesInvalidTokenOnceWithStableDedupeID(t *testing.T) {
	cache := &memoryOPPOTokenCache{token: cachedOPPOAuthToken(t, "stale-token", oppoTestNow.Add(oppoAuthTokenTTL))}
	authCalls := 0
	var sentTokens []string
	var appMessageIDs []string
	pusher := newOPPOPushForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/server/v1/auth":
			authCalls++
			writeOPPOJSON(t, w, http.StatusOK, `{"code":0,"data":{"auth_token":"fresh-token"}}`)
		case "/server/v1/message/notification/unicast":
			message := decodeOPPOMessage(t, r)
			sentTokens = append(sentTokens, r.PostForm.Get("auth_token"))
			notification := message["notification"].(map[string]interface{})
			appMessageIDs = append(appMessageIDs, notification["app_message_id"].(string))
			if len(sentTokens) == 1 {
				writeOPPOJSON(t, w, http.StatusOK, `{"code":11,"message":"Invalid AuthToken"}`)
				return
			}
			writeOPPOJSON(t, w, http.StatusOK, `{"code":0}`)
		default:
			http.NotFound(w, r)
		}
	}), cache, defaultOPPOPushOptions())

	payload := NewOPPOPayload(&PayloadInfo{Title: "title", Content: "content", ChannelID: "channel", MessageSeq: 8}, "8")
	require.NoError(t, pusher.Push("registration-id", payload))
	assert.Equal(t, []string{"stale-token", "fresh-token"}, sentTokens)
	assert.Equal(t, 1, authCalls)
	assert.Len(t, cache.delKeys, 1)
	require.Len(t, appMessageIDs, 2)
	assert.Equal(t, appMessageIDs[0], appMessageIDs[1], "auth retry must not create a second logical notification")
}

func TestOPPOPushDoesNotRetryBusinessErrors(t *testing.T) {
	tests := []struct {
		code      int
		retryable bool
	}{
		{code: 41, retryable: false},
		{code: 54, retryable: false},
		{code: 55, retryable: true},
		{code: 10000, retryable: false},
	}

	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.code), func(t *testing.T) {
			cache := &memoryOPPOTokenCache{token: cachedOPPOAuthToken(t, "cached-token", oppoTestNow.Add(oppoAuthTokenTTL))}
			requestCount := 0
			pusher := newOPPOPushForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestCount++
				writeOPPOJSON(t, w, http.StatusOK, fmt.Sprintf(`{"code":%d,"message":"rejected"}`, tt.code))
			}), cache, defaultOPPOPushOptions())

			err := pusher.Push("registration-id", NewOPPOPayload(&PayloadInfo{Title: "t", Content: "c"}, "1"))
			require.Error(t, err)
			var apiErr *oppoAPIError
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, tt.code, apiErr.Code)
			assert.Equal(t, tt.retryable, apiErr.Retryable)
			assert.Equal(t, 1, requestCount)
			assert.Empty(t, cache.delKeys)
		})
	}
}

func TestOPPOPushRejectsMalformedResponsesAndWrongPayload(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
	}{
		{name: "non 2xx", statusCode: http.StatusBadGateway, body: `{"code":0}`},
		{name: "malformed JSON", statusCode: http.StatusOK, body: `{`},
		{name: "missing code", statusCode: http.StatusOK, body: `{"message":"missing"}`},
		{name: "invalid code type", statusCode: http.StatusOK, body: `{"code":"0"}`},
		{name: "oversized body", statusCode: http.StatusOK, body: `{"code":0,"padding":"` + strings.Repeat("x", oppoMaxResponseBytes) + `"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pusher := newOPPOPushForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeOPPOJSON(t, w, tt.statusCode, tt.body)
			}), &memoryOPPOTokenCache{token: cachedOPPOAuthToken(t, "cached-token", oppoTestNow.Add(oppoAuthTokenTTL))}, defaultOPPOPushOptions())
			err := pusher.Push("registration-id", NewOPPOPayload(&PayloadInfo{Title: "t", Content: "c"}, "1"))
			require.Error(t, err)
		})
	}

	pusher := NewOPPOPush("app-id", "app-key", "app-secret", "master-secret", nil)
	require.Error(t, pusher.Push("registration-id", &BasePayload{}))
}

func TestOPPOPushHTTPClientHasBoundedTimeoutAndNamespacedCacheKey(t *testing.T) {
	first := NewOPPOPush("app-id", "app-key-1", "app-secret", "master-secret", nil)
	second := NewOPPOPush("app-id", "app-key-2", "app-secret", "master-secret", nil)

	client, ok := first.httpClient.(*http.Client)
	require.True(t, ok)
	assert.Equal(t, 5*time.Second, client.Timeout)
	assert.NotEqual(t, first.authTokenCacheKey, second.authTokenCacheKey)
	assert.True(t, strings.HasPrefix(first.authTokenCacheKey, "oppo_auth_token:"))
}

func TestLoadOPPOPushOptionsFromEnv(t *testing.T) {
	t.Setenv("TS_PUSH_OPPO_CATEGORY", "IM")
	t.Setenv("TS_PUSH_OPPO_NOTIFY_LEVEL", "2")
	t.Setenv("TS_PUSH_OPPO_CHANNEL_ID", "messages")
	t.Setenv("TS_PUSH_OPPO_PRIVATE_MSG_TEMPLATE_ID", "private-template")
	t.Setenv("TS_PUSH_OPPO_CLICK_ACTION_TYPE", "4")
	t.Setenv("TS_PUSH_OPPO_CLICK_ACTION_ACTIVITY", "com.example.chat.MainActivity")
	t.Setenv("TS_PUSH_OPPO_CLICK_ACTION_URL", "")

	options, err := loadOPPOPushOptionsFromEnv()
	require.NoError(t, err)
	assert.Equal(t, "IM", options.category)
	assert.Equal(t, 2, options.notifyLevel)
	assert.Equal(t, "messages", options.channelID)
	assert.Equal(t, "private-template", options.privateMessageTemplateID)
	assert.Equal(t, 4, options.clickActionType)
	assert.Equal(t, "com.example.chat.MainActivity", options.clickActionActivity)
	assert.Equal(t, 86400, options.offlineTTLSeconds)
}

func TestLoadOPPOPushOptionsRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name      string
		notify    string
		clickType string
		activity  string
		url       string
	}{
		{name: "invalid notify level", notify: "99"},
		{name: "invalid click action", clickType: "3"},
		{name: "activity required", clickType: "4"},
		{name: "URL required", clickType: "2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TS_PUSH_OPPO_CATEGORY", "IM")
			t.Setenv("TS_PUSH_OPPO_NOTIFY_LEVEL", tt.notify)
			t.Setenv("TS_PUSH_OPPO_CLICK_ACTION_TYPE", tt.clickType)
			t.Setenv("TS_PUSH_OPPO_CLICK_ACTION_ACTIVITY", tt.activity)
			t.Setenv("TS_PUSH_OPPO_CLICK_ACTION_URL", tt.url)
			_, err := loadOPPOPushOptionsFromEnv()
			require.Error(t, err)
		})
	}
}

func TestOPPOPushCacheFailureFallsBackToAuthentication(t *testing.T) {
	cache := &memoryOPPOTokenCache{getErr: errors.New("redis unavailable"), setErr: errors.New("redis unavailable")}
	authCalls := 0
	pusher := newOPPOPushForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/server/v1/auth":
			authCalls++
			writeOPPOJSON(t, w, http.StatusOK, `{"code":0,"data":{"auth_token":"fresh-token"}}`)
		case "/server/v1/message/notification/unicast":
			require.NoError(t, r.ParseForm())
			require.Equal(t, "fresh-token", r.PostForm.Get("auth_token"))
			writeOPPOJSON(t, w, http.StatusOK, `{"code":0}`)
		default:
			http.NotFound(w, r)
		}
	}), cache, defaultOPPOPushOptions())

	require.NoError(t, pusher.Push("registration-id", NewOPPOPayload(&PayloadInfo{Title: "t", Content: "c"}, "1")))
	assert.Equal(t, 1, authCalls)
}

func TestOPPOPushDefaultPayloadKeepsClassificationOptIn(t *testing.T) {
	var notification map[string]interface{}
	pusher := newOPPOPushForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		message := decodeOPPOMessage(t, r)
		notification = message["notification"].(map[string]interface{})
		writeOPPOJSON(t, w, http.StatusOK, `{"code":0}`)
	}), &memoryOPPOTokenCache{token: cachedOPPOAuthToken(t, "cached-token", oppoTestNow.Add(oppoAuthTokenTTL))}, defaultOPPOPushOptions())

	require.NoError(t, pusher.Push("registration-id", NewOPPOPayload(&PayloadInfo{
		Title:      "title",
		Content:    "content",
		MessageSeq: 9,
	}, "9")))
	assert.NotContains(t, notification, "category")
	assert.NotContains(t, notification, "notify_level")
	assert.NotContains(t, notification, "private_msg_template_id")
	assert.NotContains(t, notification, "channel_id")
	assert.Equal(t, float64(0), notification["click_action_type"])
	assert.Equal(t, float64(oppoDefaultOfflineTTL), notification["off_line_ttl"])
}

func TestOPPOPushValidatesInputBeforeNetwork(t *testing.T) {
	pusher := NewOPPOPush("app-id", "app-key", "app-secret", "master-secret", nil)
	pusher.configErr = nil
	pusher.tokenCache = &memoryOPPOTokenCache{token: cachedOPPOAuthToken(t, "cached-token", time.Now().Add(oppoAuthTokenTTL))}
	pusher.httpClient = oppoDoerFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid input must not reach the network")
		return nil, nil
	})

	tests := []struct {
		name        string
		deviceToken string
		payload     Payload
	}{
		{name: "wrong payload type", deviceToken: "registration-id", payload: &BasePayload{}},
		{name: "empty registration ID", payload: NewOPPOPayload(&PayloadInfo{MessageSeq: 1}, "1")},
		{name: "oversized action parameters", deviceToken: "registration-id", payload: NewOPPOPayload(&PayloadInfo{ChannelID: strings.Repeat("界", 1400), MessageSeq: 1}, "1")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Error(t, pusher.Push(tt.deviceToken, tt.payload))
		})
	}
}

func TestOPPOPushRejectsAuthFailures(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantAPIErr bool
	}{
		{name: "OPPO rejection", body: `{"code":41,"message":"bad credentials"}`, wantAPIErr: true},
		{name: "missing data", body: `{"code":0}`},
		{name: "invalid data", body: `{"code":0,"data":"invalid"}`},
		{name: "missing token", body: `{"code":0,"data":{}}`},
		{name: "empty token", body: `{"code":0,"data":{"auth_token":"  "}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			unicastCalls := 0
			pusher := newOPPOPushForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/server/v1/message/notification/unicast" {
					unicastCalls++
				}
				writeOPPOJSON(t, w, http.StatusOK, tt.body)
			}), &memoryOPPOTokenCache{}, defaultOPPOPushOptions())

			err := pusher.Push("registration-id", NewOPPOPayload(&PayloadInfo{Title: "t", Content: "c", MessageSeq: 1}, "1"))
			require.Error(t, err)
			assert.Zero(t, unicastCalls)
			if tt.wantAPIErr {
				var apiErr *oppoAPIError
				require.ErrorAs(t, err, &apiErr)
				assert.Equal(t, 41, apiErr.Code)
			}
		})
	}
}

func TestOPPOPushRetriesInvalidTokenOnlyOnce(t *testing.T) {
	cache := &memoryOPPOTokenCache{token: cachedOPPOAuthToken(t, "stale-token", oppoTestNow.Add(oppoAuthTokenTTL))}
	authCalls := 0
	unicastCalls := 0
	pusher := newOPPOPushForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/server/v1/auth":
			authCalls++
			writeOPPOJSON(t, w, http.StatusOK, `{"code":0,"data":{"auth_token":"fresh-token"}}`)
		case "/server/v1/message/notification/unicast":
			unicastCalls++
			writeOPPOJSON(t, w, http.StatusOK, `{"code":11,"message":"Invalid AuthToken"}`)
		}
	}), cache, defaultOPPOPushOptions())

	err := pusher.Push("registration-id", NewOPPOPayload(&PayloadInfo{Title: "t", Content: "c", MessageSeq: 1}, "1"))
	require.Error(t, err)
	var apiErr *oppoAPIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, 11, apiErr.Code)
	assert.Equal(t, 1, authCalls)
	assert.Equal(t, 2, unicastCalls)
}

func TestOPPOPushUsesTokenAlreadyRefreshedByPeer(t *testing.T) {
	cache := &memoryOPPOTokenCache{token: cachedOPPOAuthToken(t, "stale-token", oppoTestNow.Add(oppoAuthTokenTTL))}
	var sentTokens []string
	authCalls := 0
	pusher := newOPPOPushForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/server/v1/auth":
			authCalls++
			writeOPPOJSON(t, w, http.StatusOK, `{"code":0,"data":{"auth_token":"unexpected"}}`)
		case "/server/v1/message/notification/unicast":
			require.NoError(t, r.ParseForm())
			sentTokens = append(sentTokens, r.PostForm.Get("auth_token"))
			if len(sentTokens) == 1 {
				cache.mu.Lock()
				cache.token = cachedOPPOAuthToken(t, "peer-refreshed-token", oppoTestNow.Add(oppoAuthTokenTTL))
				cache.mu.Unlock()
				writeOPPOJSON(t, w, http.StatusOK, `{"code":11}`)
				return
			}
			writeOPPOJSON(t, w, http.StatusOK, `{"code":0}`)
		}
	}), cache, defaultOPPOPushOptions())

	require.NoError(t, pusher.Push("registration-id", NewOPPOPayload(&PayloadInfo{Title: "t", Content: "c", MessageSeq: 1}, "1")))
	assert.Equal(t, []string{"stale-token", "peer-refreshed-token"}, sentTokens)
	assert.Zero(t, authCalls)
	assert.Empty(t, cache.delKeys)
}

func TestOPPOPushReturnsTransportAndRequestConstructionErrors(t *testing.T) {
	t.Run("transport", func(t *testing.T) {
		pusher := NewOPPOPush("app-id", "app-key", "app-secret", "master-secret", nil)
		pusher.configErr = nil
		pusher.tokenCache = &memoryOPPOTokenCache{token: cachedOPPOAuthToken(t, "cached-token", time.Now().Add(oppoAuthTokenTTL))}
		pusher.httpClient = oppoDoerFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("network unavailable")
		})
		err := pusher.Push("registration-id", NewOPPOPayload(&PayloadInfo{MessageSeq: 1}, "1"))
		require.ErrorContains(t, err, "network unavailable")
	})

	t.Run("invalid URL", func(t *testing.T) {
		pusher := NewOPPOPush("app-id", "app-key", "app-secret", "master-secret", nil)
		pusher.configErr = nil
		pusher.tokenCache = &memoryOPPOTokenCache{token: cachedOPPOAuthToken(t, "cached-token", time.Now().Add(oppoAuthTokenTTL))}
		pusher.notificationUnicastURL = "://invalid"
		err := pusher.Push("registration-id", NewOPPOPayload(&PayloadInfo{MessageSeq: 1}, "1"))
		require.ErrorContains(t, err, "create OPPO request")
	})
}

func TestOPPOPushLogsProviderBusinessResult(t *testing.T) {
	t.Run("rejection", func(t *testing.T) {
		logger := &recordingOPPOLogger{}
		pusher := newOPPOPushForTest(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeOPPOJSON(t, w, http.StatusOK, `{"code":54,"message":"template rejected"}`)
		}), &memoryOPPOTokenCache{token: cachedOPPOAuthToken(t, "cached-token", oppoTestNow.Add(oppoAuthTokenTTL))}, defaultOPPOPushOptions())
		pusher.Log = logger

		err := pusher.Push("registration-id", NewOPPOPayload(&PayloadInfo{Title: "t", Content: "c", MessageSeq: 1}, "1"))
		require.Error(t, err)
		warnings, _ := logger.snapshot()
		require.Len(t, warnings, 1)
		assert.Equal(t, "OPPO push rejected", warnings[0].message)
		assert.Equal(t, int64(54), warnings[0].fields["oppo_code"])
		assert.Equal(t, false, warnings[0].fields["retryable"])
		assert.NotContains(t, warnings[0].fields, "auth_token")
		assert.NotContains(t, warnings[0].fields, "registration_id")
	})

	t.Run("accepted message ID", func(t *testing.T) {
		logger := &recordingOPPOLogger{}
		pusher := newOPPOPushForTest(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeOPPOJSON(t, w, http.StatusOK, `{"code":0,"data":{"messageId":"provider-message-id"}}`)
		}), &memoryOPPOTokenCache{token: cachedOPPOAuthToken(t, "cached-token", oppoTestNow.Add(oppoAuthTokenTTL))}, defaultOPPOPushOptions())
		pusher.Log = logger

		require.NoError(t, pusher.Push("registration-id", NewOPPOPayload(&PayloadInfo{Title: "t", Content: "c", MessageSeq: 1}, "1")))
		_, debugs := logger.snapshot()
		require.Len(t, debugs, 1)
		assert.Equal(t, "OPPO push accepted", debugs[0].message)
		assert.Equal(t, "provider-message-id", debugs[0].fields["oppo_message_id"])
	})
}

func TestOPPOAPIErrorFormatting(t *testing.T) {
	assert.Equal(t, "OPPO push failed with code 54: template rejected", (&oppoAPIError{
		Operation: "push",
		Code:      54,
		Message:   "template rejected",
	}).Error())
	assert.Equal(t, "OPPO auth failed with code -1", (&oppoAPIError{
		Operation: "auth",
		Code:      -1,
	}).Error())
}

func TestNewOPPOPushRejectsMissingCredentials(t *testing.T) {
	pusher := NewOPPOPush("", "", "", "", nil)
	require.Error(t, pusher.configErr)
}
