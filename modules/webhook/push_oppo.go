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
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"go.uber.org/zap"
)

const (
	oppoAPIBaseURL             = "https://api-push-cn.heytapmobi.com"
	oppoAuthURL                = oppoAPIBaseURL + "/server/v1/auth"
	oppoNotificationUnicastURL = oppoAPIBaseURL + "/server/v1/message/notification/unicast"

	oppoHTTPTimeout       = 5 * time.Second
	oppoAuthTokenTTL      = 20 * time.Hour
	oppoMaxResponseBytes  = 1 << 20
	oppoDefaultOfflineTTL = 24 * 60 * 60
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
	localAuthToken string
	log.Log
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
		appID:                  appID,
		appKey:                 appKey,
		appSecret:              appSecret,
		masterSecret:           masterSecret,
		authTokenCacheKey:      "oppo_auth_token:" + hex.EncodeToString(keyDigest[:8]),
		httpClient:             &http.Client{Timeout: oppoHTTPTimeout},
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
	notifyID    string
	spaceID     string
	channelID   string
	channelType uint8
	messageSeq  uint32
}

// NewOPPOPayload creates an OPPO payload and preserves the authoritative
// routing fields used by the Android client after a notification click.
func NewOPPOPayload(payloadInfo *PayloadInfo, notifyID string) *OPPOPayload {
	return &OPPOPayload{
		Payload:     payloadInfo.toPayload(),
		notifyID:    notifyID,
		spaceID:     payloadInfo.SpaceID,
		channelID:   payloadInfo.ChannelID,
		channelType: payloadInfo.ChannelType,
		messageSeq:  payloadInfo.MessageSeq,
	}
}

// GetPayload builds the OPPO payload from the canonical offline-message data.
func (o *OPPOPush) GetPayload(msg msgOfflineNotify, ctx *config.Context, toUser *user.Resp) (Payload, error) {
	payloadInfo, err := ParsePushInfo(msg, ctx, toUser)
	if err != nil {
		return nil, err
	}
	return NewOPPOPayload(payloadInfo, fmt.Sprintf("%d", msg.MessageSeq)), nil
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
	NotifyID                 uint32 `json:"notify_id"`
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
	if strings.TrimSpace(deviceToken) == "" {
		return nil, errors.New("OPPO push: empty registration ID")
	}

	notifyID, err := strconv.ParseUint(payload.notifyID, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("OPPO push: invalid notify ID %q: %w", payload.notifyID, err)
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

	dedupeSource := strings.Join([]string{
		deviceToken,
		payload.spaceID,
		payload.channelID,
		strconv.Itoa(int(payload.channelType)),
		strconv.FormatUint(uint64(payload.messageSeq), 10),
		payload.notifyID,
	}, "\x00")
	dedupeDigest := sha256.Sum256([]byte(dedupeSource))

	message := oppoUnicastMessage{
		TargetType:           2,
		TargetValue:          deviceToken,
		VerifyRegistrationID: true,
		Notification: oppoNotification{
			AppMessageID:             "octo-" + hex.EncodeToString(dedupeDigest[:16]),
			Title:                    payload.GetTitle(),
			Content:                  payload.GetContent(),
			ClickActionType:          o.options.clickActionType,
			ClickActionActivity:      o.options.clickActionActivity,
			ClickActionURL:           o.options.clickActionURL,
			ActionParameters:         string(routing),
			OffLine:                  true,
			OffLineTTL:               o.options.offlineTTLSeconds,
			ChannelID:                o.options.channelID,
			NotifyID:                 uint32(notifyID),
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
		return err
	}

	refreshedToken, refreshErr := o.refreshAuthToken(authToken)
	if refreshErr != nil {
		return refreshErr
	}
	return o.sendUnicast(refreshedToken, message)
}

func (o *OPPOPush) sendUnicast(authToken string, message []byte) error {
	response, err := o.postForm(o.notificationUnicastURL, url.Values{
		"auth_token": {authToken},
		"message":    {string(message)},
	})
	if err != nil {
		o.Warn("OPPO push request failed", zap.Error(err))
		return err
	}
	return response.apiError("push")
}

func (o *OPPOPush) getAuthToken() (string, error) {
	o.authMu.Lock()
	defer o.authMu.Unlock()

	if o.tokenCache != nil {
		token, err := o.tokenCache.GetString(o.authTokenCacheKey)
		if err != nil {
			o.Warn("read OPPO auth token cache failed; falling back to direct authentication", zap.Error(err))
		} else if token != "" {
			o.localAuthToken = token
			return token, nil
		}
	}
	if o.localAuthToken != "" {
		return o.localAuthToken, nil
	}
	return o.authenticateLocked()
}

func (o *OPPOPush) refreshAuthToken(rejectedToken string) (string, error) {
	o.authMu.Lock()
	defer o.authMu.Unlock()

	if o.tokenCache != nil {
		cachedToken, err := o.tokenCache.GetString(o.authTokenCacheKey)
		if err != nil {
			o.Warn("read OPPO auth token cache during refresh failed", zap.Error(err))
		} else if cachedToken != "" && cachedToken != rejectedToken {
			o.localAuthToken = cachedToken
			return cachedToken, nil
		}
		if err := o.tokenCache.Del(o.authTokenCacheKey); err != nil {
			o.Warn("invalidate OPPO auth token cache failed", zap.Error(err))
		}
	}
	o.localAuthToken = ""
	return o.authenticateLocked()
}

func (o *OPPOPush) authenticateLocked() (string, error) {
	timestamp := o.now().UnixMilli()
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

	o.localAuthToken = data.AuthToken
	if o.tokenCache != nil {
		if err := o.tokenCache.SetAndExpire(o.authTokenCacheKey, data.AuthToken, oppoAuthTokenTTL); err != nil {
			o.Warn("cache OPPO auth token failed; using process-local token", zap.Error(err))
		}
	}
	return data.AuthToken, nil
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

type oppoAPIError struct {
	Operation string
	Code      int
	Message   string
	Retryable bool
}

func (e *oppoAPIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("OPPO %s failed with code %d", e.Operation, e.Code)
	}
	return fmt.Sprintf("OPPO %s failed with code %d: %s", e.Operation, e.Code, e.Message)
}

func isRetryableOPPOCode(code int) bool {
	return code == -1 || code == -2 || code == 55 || (code >= 500 && code < 600)
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
