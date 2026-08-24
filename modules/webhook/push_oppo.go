package webhook

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"go.uber.org/zap"
)

const (
	// OPPO domestic service endpoint: https://open.oppomobile.com/documentation/page/info?id=11235
	oppoAPIBaseURL             = "https://api-push-cn.heytapmobi.com"
	oppoAuthURL                = oppoAPIBaseURL + "/server/v1/auth"
	oppoNotificationUnicastURL = oppoAPIBaseURL + "/server/v1/message/notification/unicast"

	oppoHTTPTimeout       = 5 * time.Second
	oppoAuthTokenTTL      = 20 * time.Hour
	oppoMaxResponseBytes  = 1 << 20
	oppoDefaultOfflineTTL = 24 * 60 * 60

	// OPPO notification limits: https://open.oppomobile.com/documentation/page/info?id=11236
	// The adapter omits style, so OPPO applies standard style (1) and its 50-character content limit.
	oppoMaxTitleRunes            = 50
	oppoMaxContentRunes          = 50
	oppoMaxActionParametersRunes = 4000
	oppoMaxRegistrationIDBytes   = 256
	oppoDefaultNotificationText  = "您有一条新的消息"
)

type oppoHTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

type oppoTokenCache interface {
	GetString(key string) (string, error)
	SetAndExpire(key string, value interface{}, expire time.Duration) error
	Del(key string) error
}

type oppoContextTokenCache struct {
	ctx *config.Context
}

func (c *oppoContextTokenCache) GetString(key string) (string, error) {
	return c.ctx.GetRedisConn().GetString(key)
}

func (c *oppoContextTokenCache) SetAndExpire(key string, value interface{}, expire time.Duration) error {
	return c.ctx.GetRedisConn().SetAndExpire(key, value, expire)
}

func (c *oppoContextTokenCache) Del(key string) error {
	return c.ctx.GetRedisConn().Del(key)
}

type oppoPushOptions struct {
	category                 string
	notifyLevel              int
	channelID                string
	privateMessageTemplateID string
	clickActionType          int
	clickActionActivity      string
	clickActionURL           string
	offlineTTLSeconds        int
}

func defaultOPPOPushOptions() oppoPushOptions {
	return oppoPushOptions{
		clickActionType:   0,
		offlineTTLSeconds: oppoDefaultOfflineTTL,
	}
}

func loadOPPOPushOptionsFromEnv() (oppoPushOptions, error) {
	options := defaultOPPOPushOptions()
	options.category = strings.TrimSpace(os.Getenv("TS_PUSH_OPPO_CATEGORY"))
	options.channelID = strings.TrimSpace(os.Getenv("TS_PUSH_OPPO_CHANNEL_ID"))
	options.privateMessageTemplateID = strings.TrimSpace(os.Getenv("TS_PUSH_OPPO_PRIVATE_MSG_TEMPLATE_ID"))
	options.clickActionActivity = strings.TrimSpace(os.Getenv("TS_PUSH_OPPO_CLICK_ACTION_ACTIVITY"))
	options.clickActionURL = strings.TrimSpace(os.Getenv("TS_PUSH_OPPO_CLICK_ACTION_URL"))

	if err := validateOPPOOptionLength("category", options.category, 32); err != nil {
		return oppoPushOptions{}, err
	}
	if err := validateOPPOOptionLength("channel ID", options.channelID, 64); err != nil {
		return oppoPushOptions{}, err
	}
	if err := validateOPPOOptionLength("private message template ID", options.privateMessageTemplateID, 128); err != nil {
		return oppoPushOptions{}, err
	}
	if err := validateOPPOOptionLength("click action activity", options.clickActionActivity, 500); err != nil {
		return oppoPushOptions{}, err
	}
	if err := validateOPPOOptionLength("click action URL", options.clickActionURL, 2000); err != nil {
		return oppoPushOptions{}, err
	}

	if raw := strings.TrimSpace(os.Getenv("TS_PUSH_OPPO_NOTIFY_LEVEL")); raw != "" {
		level, err := strconv.Atoi(raw)
		if err != nil || (level != 1 && level != 2 && level != 16) {
			return oppoPushOptions{}, fmt.Errorf("invalid TS_PUSH_OPPO_NOTIFY_LEVEL %q: allowed values are 1, 2, and 16", raw)
		}
		if options.category == "" {
			return oppoPushOptions{}, errors.New("TS_PUSH_OPPO_CATEGORY is required when TS_PUSH_OPPO_NOTIFY_LEVEL is set")
		}
		options.notifyLevel = level
	} else if options.category != "" {
		options.notifyLevel = 2
	}

	if raw := strings.TrimSpace(os.Getenv("TS_PUSH_OPPO_CLICK_ACTION_TYPE")); raw != "" {
		clickType, err := strconv.Atoi(raw)
		if err != nil {
			return oppoPushOptions{}, fmt.Errorf("invalid TS_PUSH_OPPO_CLICK_ACTION_TYPE %q", raw)
		}
		options.clickActionType = clickType
	}

	switch options.clickActionType {
	case 0:
		if options.clickActionActivity != "" || options.clickActionURL != "" {
			return oppoPushOptions{}, errors.New("OPPO click action activity or URL requires a non-zero click action type")
		}
	case 1, 4:
		if options.clickActionActivity == "" {
			return oppoPushOptions{}, fmt.Errorf("TS_PUSH_OPPO_CLICK_ACTION_ACTIVITY is required for click action type %d", options.clickActionType)
		}
		if options.clickActionURL != "" {
			return oppoPushOptions{}, fmt.Errorf("TS_PUSH_OPPO_CLICK_ACTION_URL is not valid for click action type %d", options.clickActionType)
		}
	case 2, 5:
		if options.clickActionURL == "" {
			return oppoPushOptions{}, fmt.Errorf("TS_PUSH_OPPO_CLICK_ACTION_URL is required for click action type %d", options.clickActionType)
		}
		if options.clickActionActivity != "" {
			return oppoPushOptions{}, fmt.Errorf("TS_PUSH_OPPO_CLICK_ACTION_ACTIVITY is not valid for click action type %d", options.clickActionType)
		}
	default:
		return oppoPushOptions{}, fmt.Errorf("invalid TS_PUSH_OPPO_CLICK_ACTION_TYPE %d: allowed values are 0, 1, 2, 4, and 5", options.clickActionType)
	}

	return options, nil
}

func validateOPPOOptionLength(name, value string, maxRunes int) error {
	if utf8.RuneCountInString(value) > maxRunes {
		return fmt.Errorf("OPPO %s exceeds %d characters", name, maxRunes)
	}
	return nil
}

// OPPOPush implements OPPO notification unicast over the current REST API.
type OPPOPush struct {
	appID        string
	appKey       string
	appSecret    string
	masterSecret string

	authTokenCacheKey      string
	httpClient             oppoHTTPDoer
	tokenCache             oppoTokenCache
	authURL                string
	notificationUnicastURL string
	now                    func() time.Time
	options                oppoPushOptions
	configErr              error

	authMu         sync.Mutex
	localAuthToken atomic.Pointer[oppoAuthToken]
	log.Log
}

type oppoAuthToken struct {
	value     string
	expiresAt time.Time
}

type oppoCachedAuthToken struct {
	AuthToken string `json:"auth_token"`
	ExpiresAt int64  `json:"expires_at"`
}

// NewOPPOPush creates a production OPPO REST pusher. Redis is used as a shared
// token cache when a config context is supplied; a local token remains available
// as a bounded fallback when Redis is temporarily unavailable.
func NewOPPOPush(appID, appKey, appSecret, masterSecret string, ctx *config.Context) *OPPOPush {
	options, configErr := loadOPPOPushOptionsFromEnv()
	if configErr != nil {
		options = defaultOPPOPushOptions()
	}
	if strings.TrimSpace(appKey) == "" || strings.TrimSpace(masterSecret) == "" {
		credentialErr := errors.New("OPPO appKey and masterSecret are required")
		if configErr == nil {
			configErr = credentialErr
		} else {
			configErr = errors.Join(configErr, credentialErr)
		}
	}

	var tokenCache oppoTokenCache
	if ctx != nil {
		tokenCache = &oppoContextTokenCache{ctx: ctx}
	}

	keyDigest := sha256.Sum256([]byte(appKey))
	return &OPPOPush{
		appID:             appID,
		appKey:            appKey,
		appSecret:         appSecret,
		masterSecret:      masterSecret,
		authTokenCacheKey: "oppo_auth_token:" + hex.EncodeToString(keyDigest[:8]),
		httpClient: &http.Client{
			Timeout: oppoHTTPTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		tokenCache:             tokenCache,
		authURL:                oppoAuthURL,
		notificationUnicastURL: oppoNotificationUnicastURL,
		now:                    time.Now,
		options:                options,
		configErr:              configErr,
		Log:                    log.NewTLog("oppopush"),
	}
}

// OPPOPayload is the server-owned input for an OPPO notification.
type OPPOPayload struct {
	Payload
	dedupeID    string
	spaceID     string
	channelID   string
	channelType uint8
	messageSeq  uint32
}

// NewOPPOPayload creates an OPPO payload and preserves the authoritative
// routing fields used by the Android client after a notification click.
func NewOPPOPayload(payloadInfo *PayloadInfo, dedupeID string) *OPPOPayload {
	return &OPPOPayload{
		Payload:     payloadInfo.toPayload(),
		dedupeID:    dedupeID,
		spaceID:     payloadInfo.SpaceID,
		channelID:   payloadInfo.ChannelID,
		channelType: payloadInfo.ChannelType,
		messageSeq:  payloadInfo.MessageSeq,
	}
}

func newOPPOPayloadForMessage(payloadInfo *PayloadInfo, msg msgOfflineNotify) (*OPPOPayload, error) {
	var dedupeID string
	if msg.MessageID != 0 {
		dedupeID = "message:" + strconv.FormatInt(msg.MessageID, 10)
	} else if clientMsgNo := strings.TrimSpace(msg.ClientMsgNo); clientMsgNo != "" {
		dedupeID = "client:" + clientMsgNo
	} else {
		return nil, errors.New("OPPO push: missing message identity")
	}
	return NewOPPOPayload(payloadInfo, dedupeID), nil
}

// GetPayload builds the OPPO payload from the canonical offline-message data.
func (o *OPPOPush) GetPayload(msg msgOfflineNotify, ctx *config.Context, toUser *user.Resp) (Payload, error) {
	payloadInfo, err := ParsePushInfo(msg, ctx, toUser)
	if err != nil {
		return nil, err
	}
	return newOPPOPayloadForMessage(payloadInfo, msg)
}

type oppoRoutingParameters struct {
	SpaceID     string `json:"space_id,omitempty"`
	ChannelID   string `json:"channel_id,omitempty"`
	ChannelType uint8  `json:"channel_type"`
	MessageSeq  uint32 `json:"message_seq"`
}

type oppoNotification struct {
	AppMessageID             string `json:"app_message_id"`
	Title                    string `json:"title"`
	Content                  string `json:"content"`
	ClickActionType          int    `json:"click_action_type"`
	ClickActionActivity      string `json:"click_action_activity,omitempty"`
	ClickActionURL           string `json:"click_action_url,omitempty"`
	ActionParameters         string `json:"action_parameters"`
	OffLine                  bool   `json:"off_line"`
	OffLineTTL               int    `json:"off_line_ttl"`
	ChannelID                string `json:"channel_id,omitempty"`
	Category                 string `json:"category,omitempty"`
	NotifyLevel              int    `json:"notify_level,omitempty"`
	PrivateMessageTemplateID string `json:"private_msg_template_id,omitempty"`
}

type oppoUnicastMessage struct {
	TargetType           int              `json:"target_type"`
	TargetValue          string           `json:"target_value"`
	VerifyRegistrationID bool             `json:"verify_registration_id"`
	Notification         oppoNotification `json:"notification"`
}

func (o *OPPOPush) buildUnicastMessage(deviceToken string, payload *OPPOPayload) ([]byte, error) {
	if err := validateOPPORegistrationID(deviceToken); err != nil {
		return nil, err
	}

	routing, err := json.Marshal(oppoRoutingParameters{
		SpaceID:     payload.spaceID,
		ChannelID:   payload.channelID,
		ChannelType: payload.channelType,
		MessageSeq:  payload.messageSeq,
	})
	if err != nil {
		return nil, fmt.Errorf("OPPO push: encode action parameters: %w", err)
	}
	if utf8.RuneCount(routing) > oppoMaxActionParametersRunes {
		return nil, fmt.Errorf("OPPO push: action parameters exceed %d characters", oppoMaxActionParametersRunes)
	}

	dedupeSource := strings.Join([]string{
		deviceToken,
		payload.dedupeID,
	}, "\x00")
	dedupeDigest := sha256.Sum256([]byte(dedupeSource))

	message := oppoUnicastMessage{
		TargetType:           2,
		TargetValue:          deviceToken,
		VerifyRegistrationID: true,
		Notification: oppoNotification{
			AppMessageID:             "octo-" + hex.EncodeToString(dedupeDigest[:16]),
			Title:                    normalizeOPPONotificationText(payload.GetTitle(), oppoMaxTitleRunes),
			Content:                  normalizeOPPONotificationText(payload.GetContent(), oppoMaxContentRunes),
			ClickActionType:          o.options.clickActionType,
			ClickActionActivity:      o.options.clickActionActivity,
			ClickActionURL:           o.options.clickActionURL,
			ActionParameters:         string(routing),
			OffLine:                  true,
			OffLineTTL:               o.options.offlineTTLSeconds,
			ChannelID:                o.options.channelID,
			Category:                 o.options.category,
			NotifyLevel:              o.options.notifyLevel,
			PrivateMessageTemplateID: o.options.privateMessageTemplateID,
		},
	}

	encoded, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("OPPO push: encode message: %w", err)
	}
	return encoded, nil
}

func validateOPPORegistrationID(deviceToken string) error {
	if deviceToken == "" {
		return errors.New("OPPO push: empty registration ID")
	}
	if len(deviceToken) > oppoMaxRegistrationIDBytes {
		return fmt.Errorf("OPPO push: registration ID exceeds %d bytes", oppoMaxRegistrationIDBytes)
	}
	if !utf8.ValidString(deviceToken) {
		return errors.New("OPPO push: registration ID is not valid UTF-8")
	}
	for _, character := range deviceToken {
		if character == ';' || character == ',' || unicode.IsSpace(character) || unicode.IsControl(character) {
			return errors.New("OPPO push: malformed registration ID")
		}
	}
	return nil
}

func normalizeOPPONotificationText(value string, maxRunes int) string {
	if strings.TrimSpace(value) == "" {
		value = oppoDefaultNotificationText
	}
	runes := []rune(value)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes])
	}
	return value
}

// Push sends one notification. OPPO code 11 is the only response retried here:
// it means the cached auth token is invalid, so the adapter refreshes it and
// repeats the same idempotent message once.
func (o *OPPOPush) Push(deviceToken string, payload Payload) error {
	if o.configErr != nil {
		return fmt.Errorf("OPPO push configuration: %w", o.configErr)
	}

	oppoPayload, ok := payload.(*OPPOPayload)
	if !ok || oppoPayload == nil {
		return fmt.Errorf("OPPO push: unexpected payload type %T", payload)
	}
	message, err := o.buildUnicastMessage(deviceToken, oppoPayload)
	if err != nil {
		return err
	}

	authToken, err := o.getAuthToken()
	if err != nil {
		return err
	}
	err = o.sendUnicast(authToken, message)
	var apiErr *oppoAPIError
	if !errors.As(err, &apiErr) || apiErr.Code != 11 {
		o.logPushFailure(deviceToken, err)
		return err
	}
	o.Debug("OPPO auth token rejected; refreshing",
		zap.Int("oppo_code", apiErr.Code),
		zap.Bool("retryable", true),
		zap.String("device_token", maskToken(deviceToken)),
	)

	refreshedToken, refreshErr := o.refreshAuthToken(authToken)
	if refreshErr != nil {
		return refreshErr
	}
	err = o.sendUnicast(refreshedToken, message)
	o.logPushFailure(deviceToken, err)
	return err
}

func (o *OPPOPush) sendUnicast(authToken string, message []byte) error {
	response, err := o.postForm(o.notificationUnicastURL, url.Values{
		"auth_token": {authToken},
		"message":    {string(message)},
	})
	if err != nil {
		return err
	}
	if err := response.apiError("push"); err != nil {
		return err
	}
	if messageID := response.messageID(); messageID != "" {
		o.Debug("OPPO push accepted", zap.String("oppo_message_id", messageID))
	}
	return nil
}

func (o *OPPOPush) logPushFailure(deviceToken string, err error) {
	if err == nil {
		return
	}
	fields := []zap.Field{zap.String("device_token", maskToken(deviceToken))}
	var apiErr *oppoAPIError
	if errors.As(err, &apiErr) {
		fields = append(fields, zap.Int("oppo_code", apiErr.Code), zap.Bool("retryable", apiErr.Retryable))
		if apiErr.Code == 41 {
			o.Debug("OPPO push rejected", fields...)
			return
		}
	} else {
		fields = append(fields, zap.Error(err))
	}
	o.Warn("OPPO push rejected", fields...)
}

func (o *OPPOPush) getAuthToken() (string, error) {
	if token, ok := o.processLocalAuthToken(); ok {
		return token, nil
	}

	o.authMu.Lock()
	defer o.authMu.Unlock()

	if token, ok := o.processLocalAuthToken(); ok {
		return token, nil
	}
	if o.tokenCache != nil {
		cached, err := o.tokenCache.GetString(o.authTokenCacheKey)
		if err != nil {
			o.Warn("read OPPO auth token cache failed; falling back to direct authentication", zap.Error(err))
		} else if cached != "" {
			token, parseErr := parseOPPOCachedAuthToken(cached, o.now())
			if parseErr == nil {
				o.localAuthToken.Store(token)
				return token.value, nil
			}
			o.Warn("discard invalid OPPO auth token cache", zap.Error(parseErr))
			o.deleteCachedAuthToken()
		}
	}
	return o.authenticateLocked()
}

func (o *OPPOPush) refreshAuthToken(rejectedToken string) (string, error) {
	o.authMu.Lock()
	defer o.authMu.Unlock()

	if token, ok := o.processLocalAuthToken(); ok && token != rejectedToken {
		return token, nil
	}
	if o.tokenCache != nil {
		cached, err := o.tokenCache.GetString(o.authTokenCacheKey)
		if err != nil {
			o.Warn("read OPPO auth token cache during refresh failed", zap.Error(err))
		} else if cached != "" {
			cachedToken, parseErr := parseOPPOCachedAuthToken(cached, o.now())
			if parseErr == nil && cachedToken.value != rejectedToken {
				o.localAuthToken.Store(cachedToken)
				return cachedToken.value, nil
			}
			o.deleteCachedAuthToken()
		}
	}
	o.localAuthToken.Store(nil)
	return o.authenticateLocked()
}

func (o *OPPOPush) processLocalAuthToken() (string, bool) {
	token := o.localAuthToken.Load()
	if token == nil || token.value == "" || !o.now().Before(token.expiresAt) {
		return "", false
	}
	return token.value, true
}

func parseOPPOCachedAuthToken(value string, now time.Time) (*oppoAuthToken, error) {
	var cached oppoCachedAuthToken
	if err := json.Unmarshal([]byte(value), &cached); err != nil {
		return nil, fmt.Errorf("decode cached token: %w", err)
	}
	cached.AuthToken = strings.TrimSpace(cached.AuthToken)
	if cached.AuthToken == "" || cached.ExpiresAt <= 0 {
		return nil, errors.New("cached token is missing auth_token or expires_at")
	}
	token := &oppoAuthToken{
		value:     cached.AuthToken,
		expiresAt: time.UnixMilli(cached.ExpiresAt),
	}
	if !now.Before(token.expiresAt) {
		return nil, errors.New("cached token has expired")
	}
	return token, nil
}

func (o *OPPOPush) deleteCachedAuthToken() {
	if o.tokenCache == nil {
		return
	}
	if err := o.tokenCache.Del(o.authTokenCacheKey); err != nil {
		o.Warn("invalidate OPPO auth token cache failed", zap.Error(err))
	}
}

func (o *OPPOPush) authenticateLocked() (string, error) {
	issuedAt := o.now()
	timestamp := issuedAt.UnixMilli()
	sign := o.SHA256(fmt.Sprintf("%s%d%s", o.appKey, timestamp, o.masterSecret))
	response, err := o.postForm(o.authURL, url.Values{
		"app_key":   {o.appKey},
		"sign":      {sign},
		"timestamp": {strconv.FormatInt(timestamp, 10)},
	})
	if err != nil {
		return "", fmt.Errorf("OPPO auth request: %w", err)
	}
	if err := response.apiError("auth"); err != nil {
		var apiErr *oppoAPIError
		if errors.As(err, &apiErr) {
			o.Warn("OPPO auth rejected", zap.Int("oppo_code", apiErr.Code), zap.Bool("retryable", apiErr.Retryable))
		}
		return "", err
	}

	var data struct {
		AuthToken string `json:"auth_token"`
	}
	if len(response.Data) == 0 || string(response.Data) == "null" {
		return "", errors.New("OPPO auth: missing data")
	}
	if err := json.Unmarshal(response.Data, &data); err != nil {
		return "", fmt.Errorf("OPPO auth: decode data: %w", err)
	}
	if strings.TrimSpace(data.AuthToken) == "" {
		return "", errors.New("OPPO auth: missing auth_token")
	}

	token := &oppoAuthToken{
		value:     data.AuthToken,
		expiresAt: issuedAt.Add(oppoAuthTokenTTL),
	}
	o.localAuthToken.Store(token)
	if o.tokenCache != nil {
		cached, err := json.Marshal(oppoCachedAuthToken{
			AuthToken: token.value,
			ExpiresAt: token.expiresAt.UnixMilli(),
		})
		if err != nil {
			return "", fmt.Errorf("OPPO auth: encode token cache: %w", err)
		}
		if err := o.tokenCache.SetAndExpire(o.authTokenCacheKey, string(cached), oppoAuthTokenTTL); err != nil {
			o.Warn("cache OPPO auth token failed; using process-local token", zap.Error(err))
		}
	}
	return token.value, nil
}

type oppoAPIResponse struct {
	Code    *int            `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (r *oppoAPIResponse) apiError(operation string) error {
	if r == nil || r.Code == nil {
		return fmt.Errorf("OPPO %s: response missing code", operation)
	}
	if *r.Code == 0 {
		return nil
	}
	return &oppoAPIError{
		Operation: operation,
		Code:      *r.Code,
		Message:   r.Message,
		Retryable: isRetryableOPPOCode(*r.Code),
	}
}

func (r *oppoAPIResponse) messageID() string {
	if r == nil || len(r.Data) == 0 || string(r.Data) == "null" {
		return ""
	}
	var data struct {
		MessageID string `json:"messageId"`
	}
	if err := json.Unmarshal(r.Data, &data); err != nil {
		return ""
	}
	return strings.TrimSpace(data.MessageID)
}

type oppoAPIError struct {
	Operation string
	Code      int
	Message   string
	Retryable bool
}

func (e *oppoAPIError) Error() string {
	return fmt.Sprintf("OPPO %s failed with code %d", e.Operation, e.Code)
}

func isRetryableOPPOCode(code int) bool {
	return code == -1 || code == -2 || code == 11 || code == 55 || (code >= 500 && code < 600)
}

func (o *OPPOPush) postForm(endpoint string, values url.Values) (*oppoAPIResponse, error) {
	body := values.Encode()
	request, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create OPPO request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	request.Header.Set("Accept", "application/json")

	response, err := o.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("execute OPPO request: %w", err)
	}
	defer func() {
		if closeErr := response.Body.Close(); closeErr != nil {
			o.Warn("close OPPO response body failed", zap.Error(closeErr))
		}
	}()

	limitedBody, err := io.ReadAll(io.LimitReader(response.Body, oppoMaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read OPPO response: %w", err)
	}
	if len(limitedBody) > oppoMaxResponseBytes {
		return nil, fmt.Errorf("OPPO response exceeds %d bytes", oppoMaxResponseBytes)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("OPPO HTTP request failed with status %d", response.StatusCode)
	}

	decoder := json.NewDecoder(bytes.NewReader(limitedBody))
	var decoded oppoAPIResponse
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode OPPO response: %w", err)
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("decode OPPO response: multiple JSON values")
		}
		return nil, fmt.Errorf("decode OPPO response trailing data: %w", err)
	}
	if decoded.Code == nil {
		return nil, errors.New("decode OPPO response: missing code")
	}
	return &decoded, nil
}

// SHA256 returns the lowercase hex digest required by the OPPO auth API.
func (o *OPPOPush) SHA256(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

// parseOPPOAuthResponse is kept as a compatibility seam for existing unit
// tests and callers that decode through octo-lib's generic response map.
func parseOPPOAuthResponse(resp map[string]interface{}) (string, error) {
	if resp == nil || resp["code"] == nil {
		return "", errors.New("OPPO auth: empty response")
	}
	codeNumber, ok := resp["code"].(json.Number)
	if !ok {
		return "", fmt.Errorf("OPPO auth: unexpected code type %T", resp["code"])
	}
	code, err := codeNumber.Int64()
	if err != nil {
		return "", fmt.Errorf("OPPO auth: invalid code: %w", err)
	}
	if code != 0 {
		message, _ := resp["message"].(string)
		return "", fmt.Errorf("OPPO auth failed: code=%d, message=%s", code, message)
	}
	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("OPPO auth: unexpected data type %T", resp["data"])
	}
	token, ok := data["auth_token"].(string)
	if !ok || strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("OPPO auth: unexpected auth_token type %T", data["auth_token"])
	}
	return token, nil
}
