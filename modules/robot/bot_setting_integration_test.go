package robot

// task bot-setting-store —— owner 配置端点的集成测试（需要 MySQL + Redis）。
// 纯函数层的解析优先级在 bot_setting_test.go；这里覆盖端点行为：目录回显、
// 三层落点、删除即回落、属主守卫、派生键不可写。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	commonmodule "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/go-redis/redis"
	"github.com/stretchr/testify/assert"
)

const (
	settingsRobotID      = "bot_settings_owner"
	settingsOtherRobotID = "bot_settings_other"
)

// resetSettingsUIDRateLimit clears the shared per-UID bucket.
//
// The settings routes mount SharedUIDRateLimiter, and that bucket lives in
// Redis — CleanAllTables does not touch it, so without this a run of several
// cases in one package would start tripping 429 partway through and the
// failure would look like an assertion bug.
func resetSettingsUIDRateLimit(t *testing.T, ctx *config.Context) {
	t.Helper()
	client := redis.NewClient(&redis.Options{
		Addr:     ctx.GetConfig().DB.RedisAddr,
		Password: ctx.GetConfig().DB.RedisPass,
	})
	defer client.Close()
	keys, err := client.Keys("ratelimit:uid:*").Result()
	if err == nil && len(keys) > 0 {
		_ = client.Del(keys...).Err()
	}
}

// setupBotSettings wires a bot owned by the test caller plus one owned by
// somebody else, so the ownership guard has a real negative case.
func setupBotSettings(t *testing.T) (http.Handler, *config.Context) {
	t.Helper()
	s, ctx := testutil.NewTestServer()
	assert.NoError(t, testutil.CleanAllTables(ctx))
	resetSettingsUIDRateLimit(t, ctx)
	// SystemSettings is a process-wide singleton holding an in-memory snapshot
	// that only refreshes on its own timer. CleanAllTables truncates the
	// system_setting rows but leaves that snapshot alone, so without this a
	// case that writes a deployment default leaks it into every later case in
	// the binary — which surfaces as source="global" where "default" was
	// expected, and only under -shuffle. Same family as the rate-limit bucket
	// above: CleanAllTables clears tables, not in-process caches.
	assert.NoError(t, commonmodule.EnsureSystemSettings(ctx).Reload())

	_, err := ctx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, status, creator_uid) VALUES (?, 1, ?)",
		settingsRobotID, testutil.UID,
	).Exec()
	assert.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, status, creator_uid) VALUES (?, 1, ?)",
		settingsOtherRobotID, "someone_else",
	).Exec()
	assert.NoError(t, err)
	return s.GetRoute(), ctx
}

// settingsItem mirrors one row of the owner catalog response. Value is a
// pointer so the test can tell an explicit override apart from "unset" —
// the same distinction the UI needs to render "restore default".
type settingsItem struct {
	Key       string `json:"key"`
	Type      string `json:"type"`
	Value     *bool  `json:"value"`
	Effective bool   `json:"effective_value"`
	Source    string `json:"source"`
	Editable  bool   `json:"editable"`
}

type settingsListResp struct {
	List []settingsItem `json:"list"`
}

func getSettings(t *testing.T, handler http.Handler, robotID string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/robot/"+robotID+"/settings", nil)
	assert.NoError(t, err)
	req.Header.Set("token", token)
	handler.ServeHTTP(w, req)
	return w
}

func putSettings(t *testing.T, handler http.Handler, robotID string, items ...map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]interface{}{"items": items})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, err := http.NewRequest("PUT", "/v1/robot/"+robotID+"/settings", bytes.NewReader(body))
	assert.NoError(t, err)
	req.Header.Set("token", token)
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(w, req)
	return w
}

func deleteSetting(t *testing.T, handler http.Handler, robotID, key string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, err := http.NewRequest("DELETE", "/v1/robot/"+robotID+"/settings/"+key, nil)
	assert.NoError(t, err)
	req.Header.Set("token", token)
	handler.ServeHTTP(w, req)
	return w
}

func findSetting(t *testing.T, resp settingsListResp, key string) settingsItem {
	t.Helper()
	for _, item := range resp.List {
		if item.Key == key {
			return item
		}
	}
	t.Fatalf("key %s missing from the settings catalog", key)
	return settingsItem{}
}

// TestBotSettings_CatalogListsEveryRegisteredKey pins that the read endpoint is
// a catalog, not just the rows that happen to exist: a client renders the Bot
// management screen from this response, so a key with no override must still
// appear (with value=null) rather than vanish.
func TestBotSettings_CatalogListsEveryRegisteredKey(t *testing.T) {
	handler, _ := setupBotSettings(t)

	w := getSettings(t, handler, settingsRobotID)
	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var resp settingsListResp
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Len(t, resp.List, len(botSettingDefs))

	for _, key := range []string{
		BotSettingKeyCardEnabled, BotSettingKeyDisplayEnabled,
		BotSettingKeyInteractionEnabled, BotSettingKeyReasoningEnabled,
	} {
		item := findSetting(t, resp, key)
		assert.Nil(t, item.Value, "%s has no override yet → value must be null", key)
		assert.Equal(t, botSettingTypeBool, item.Type)
	}

	// card_enabled is derived from the deployment env, so it is reported as a
	// read-only entry the UI can use to grey out the rest.
	cardEnabled := findSetting(t, resp, BotSettingKeyCardEnabled)
	assert.False(t, cardEnabled.Editable, "card_enabled must be read-only")
	assert.Equal(t, botSettingSourceEnv, cardEnabled.Source)

	// The three owner-editable switches start on the code default.
	reasoning := findSetting(t, resp, BotSettingKeyReasoningEnabled)
	assert.True(t, reasoning.Editable)
	assert.Equal(t, botSettingSourceDefault, reasoning.Source)
	assert.True(t, reasoning.Effective, "code default is true")

	// Per-bot private config must never be held by a shared cache. Vary names
	// the header this route actually authenticates with — user session is
	// `token`, not Authorization (that is the bot-token profile endpoint).
	cacheControl := w.Header().Get("Cache-Control")
	assert.Contains(t, cacheControl, "private")
	assert.Contains(t, cacheControl, "no-store")
	assert.Equal(t, "token", w.Header().Get("Vary"))
}

// TestBotSettings_OverrideThenDeleteFallsBack walks the full lifecycle the
// "restore default" control depends on: an override reports source=bot, and
// deleting it returns to the layer underneath rather than latching false.
func TestBotSettings_OverrideThenDeleteFallsBack(t *testing.T) {
	handler, _ := setupBotSettings(t)

	w := putSettings(t, handler, settingsRobotID,
		map[string]any{"key": BotSettingKeyReasoningEnabled, "value": "false"})
	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var resp settingsListResp
	assert.NoError(t, json.Unmarshal(getSettings(t, handler, settingsRobotID).Body.Bytes(), &resp))
	item := findSetting(t, resp, BotSettingKeyReasoningEnabled)
	assert.NotNil(t, item.Value)
	assert.False(t, *item.Value, "explicit override must be echoed back")
	assert.False(t, item.Effective)
	assert.Equal(t, botSettingSourceBot, item.Source)

	// Deleting the override falls back to the layer below (here: code default),
	// which is a different outcome from "set to false".
	w = deleteSetting(t, handler, settingsRobotID, BotSettingKeyReasoningEnabled)
	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	assert.NoError(t, json.Unmarshal(getSettings(t, handler, settingsRobotID).Body.Bytes(), &resp))
	item = findSetting(t, resp, BotSettingKeyReasoningEnabled)
	assert.Nil(t, item.Value, "override was deleted → value must be null again")
	assert.True(t, item.Effective)
	assert.Equal(t, botSettingSourceDefault, item.Source)

	// Idempotent: deleting an override that no longer exists still succeeds.
	w = deleteSetting(t, handler, settingsRobotID, BotSettingKeyReasoningEnabled)
	assert.Equal(t, http.StatusOK, w.Code, "delete must be idempotent")
}

// TestBotSettings_GlobalDefaultLayer pins the middle tier: with no per-bot row,
// the deployment default decides, and source says so — the distinction the UI
// needs to render "following the server default".
func TestBotSettings_GlobalDefaultLayer(t *testing.T) {
	handler, ctx := setupBotSettings(t)

	_, err := ctx.DB().InsertBySql(
		"INSERT INTO system_setting (category, key_name, value, value_type) VALUES (?, ?, ?, 'bool')",
		"botcard", "reasoning_enabled", "0",
	).Exec()
	assert.NoError(t, err)
	// The snapshot is refreshed on a timer; force it so the test does not wait
	// out defaultReloadTTL.
	assert.NoError(t, commonmodule.EnsureSystemSettings(ctx).Reload())

	var resp settingsListResp
	assert.NoError(t, json.Unmarshal(getSettings(t, handler, settingsRobotID).Body.Bytes(), &resp))
	item := findSetting(t, resp, BotSettingKeyReasoningEnabled)
	assert.Nil(t, item.Value, "no per-bot override")
	assert.False(t, item.Effective, "global default decides")
	assert.Equal(t, botSettingSourceGlobal, item.Source)

	// A per-bot override outranks the global default even when it disagrees.
	w := putSettings(t, handler, settingsRobotID,
		map[string]any{"key": BotSettingKeyReasoningEnabled, "value": "true"})
	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	assert.NoError(t, json.Unmarshal(getSettings(t, handler, settingsRobotID).Body.Bytes(), &resp))
	item = findSetting(t, resp, BotSettingKeyReasoningEnabled)
	assert.True(t, item.Effective, "bot override wins over the global default")
	assert.Equal(t, botSettingSourceBot, item.Source)
}

// TestBotSettings_WriteRejections covers every way a write must be refused.
// The derived-key case is the load-bearing one: accepting it would let the DB
// claim cards are on while the deployment env has them off.
func TestBotSettings_WriteRejections(t *testing.T) {
	handler, _ := setupBotSettings(t)

	cases := []struct {
		name string
		item map[string]any
	}{
		{"unregistered key", map[string]any{"key": "bot.not_a_real_key", "value": "true"}},
		{"derived key is read-only", map[string]any{"key": BotSettingKeyCardEnabled, "value": "true"}},
		{"illegal bool literal", map[string]any{"key": BotSettingKeyDisplayEnabled, "value": "maybe"}},
		{"empty value", map[string]any{"key": BotSettingKeyDisplayEnabled, "value": ""}},
		{"null value", map[string]any{"key": BotSettingKeyDisplayEnabled, "value": nil}},
		{"non-bool JSON", map[string]any{"key": BotSettingKeyDisplayEnabled, "value": 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := putSettings(t, handler, settingsRobotID, tc.item)
			assert.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
		})
	}

	// A batch containing one bad item must not half-apply the good ones.
	w := putSettings(t, handler, settingsRobotID,
		map[string]any{"key": BotSettingKeyDisplayEnabled, "value": "false"},
		map[string]any{"key": "bot.not_a_real_key", "value": "true"})
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var resp settingsListResp
	assert.NoError(t, json.Unmarshal(getSettings(t, handler, settingsRobotID).Body.Bytes(), &resp))
	assert.Nil(t, findSetting(t, resp, BotSettingKeyDisplayEnabled).Value,
		"a rejected batch must not have written its valid items")
}

// TestBotSettings_OwnershipGuard pins that these endpoints are owner-only, and
// that an unknown bot is 404 rather than 403 (the latter would confirm the id
// exists).
func TestBotSettings_OwnershipGuard(t *testing.T) {
	handler, _ := setupBotSettings(t)

	t.Run("read someone else's bot is forbidden", func(t *testing.T) {
		w := getSettings(t, handler, settingsOtherRobotID)
		assert.Equal(t, http.StatusForbidden, w.Code, "body=%s", w.Body.String())
	})
	t.Run("write someone else's bot is forbidden", func(t *testing.T) {
		w := putSettings(t, handler, settingsOtherRobotID,
			map[string]any{"key": BotSettingKeyReasoningEnabled, "value": "false"})
		assert.Equal(t, http.StatusForbidden, w.Code, "body=%s", w.Body.String())
	})
	t.Run("delete on someone else's bot is forbidden", func(t *testing.T) {
		w := deleteSetting(t, handler, settingsOtherRobotID, BotSettingKeyReasoningEnabled)
		assert.Equal(t, http.StatusForbidden, w.Code, "body=%s", w.Body.String())
	})
	t.Run("unknown bot is not found", func(t *testing.T) {
		w := getSettings(t, handler, "bot_does_not_exist")
		assert.Equal(t, http.StatusNotFound, w.Code, "body=%s", w.Body.String())
	})

	// Ownership is decided before per-item validation, so a malformed request
	// against a bot you do not own answers 403/404 rather than 400. Two things
	// depend on that ordering: the endpoint's documented ownership semantics,
	// and not letting a non-owner tell "registered and writable" (403) apart
	// from "unknown key" (400) — which would enumerate the server-side registry.
	t.Run("a malformed write on someone else's bot is still forbidden", func(t *testing.T) {
		w := putSettings(t, handler, settingsOtherRobotID,
			map[string]any{"key": "bot.not_a_real_key", "value": "nonsense"})
		assert.Equal(t, http.StatusForbidden, w.Code, "body=%s", w.Body.String())
	})
	t.Run("a malformed write on an unknown bot is still not found", func(t *testing.T) {
		w := putSettings(t, handler, "bot_does_not_exist",
			map[string]any{"key": "bot.not_a_real_key", "value": "nonsense"})
		assert.Equal(t, http.StatusNotFound, w.Code, "body=%s", w.Body.String())
	})
	t.Run("an unknown key on someone else's bot is forbidden, not bad request", func(t *testing.T) {
		w := deleteSetting(t, handler, settingsOtherRobotID, "bot.not_a_real_key")
		assert.Equal(t, http.StatusForbidden, w.Code, "body=%s", w.Body.String())
	})
}

// TestBotSettings_CatalogValueRoundTrips pins that what the read endpoint emits
// can be written straight back.
//
// The catalog serves `value` as a JSON bool, and the obvious client flow is
// "read the catalog, flip one entry, PUT the item back". If the write side only
// accepted strings that round trip would 400 at BindJSON and every caller would
// have to know to stringify — so the write side accepts both shapes.
func TestBotSettings_CatalogValueRoundTrips(t *testing.T) {
	handler, _ := setupBotSettings(t)

	// Write with a real JSON bool, exactly as the read endpoint emits it.
	w := putSettings(t, handler, settingsRobotID,
		map[string]any{"key": BotSettingKeyDisplayEnabled, "value": false})
	assert.Equal(t, http.StatusOK, w.Code, "a JSON bool must be accepted; body=%s", w.Body.String())

	var resp settingsListResp
	assert.NoError(t, json.Unmarshal(getSettings(t, handler, settingsRobotID).Body.Bytes(), &resp))
	item := findSetting(t, resp, BotSettingKeyDisplayEnabled)
	assert.NotNil(t, item.Value)
	assert.False(t, *item.Value)
	assert.Equal(t, botSettingSourceBot, item.Source)

	// Feed the echoed value straight back — the round trip a UI performs.
	w = putSettings(t, handler, settingsRobotID,
		map[string]any{"key": item.Key, "value": *item.Value})
	assert.Equal(t, http.StatusOK, w.Code, "echoed value must round trip; body=%s", w.Body.String())
}

// TestBotSettings_BatchValidationRejectsAtomically covers the validation half of
// the all-or-nothing contract: one bad item rejects the whole batch.
//
// Named for what it actually covers. It does NOT exercise the transaction —
// validation returns before Begin() is reached, so this case passes against the
// pre-transaction code too. The rollback itself is covered by
// TestWriteBotSettingOverrides_RollsBackOnMidBatchFailure.
func TestBotSettings_BatchValidationRejectsAtomically(t *testing.T) {
	handler, _ := setupBotSettings(t)

	w := putSettings(t, handler, settingsRobotID,
		map[string]any{"key": BotSettingKeyDisplayEnabled, "value": false},
		map[string]any{"key": BotSettingKeyInteractionEnabled, "value": false},
		map[string]any{"key": BotSettingKeyReasoningEnabled, "value": "not-a-bool"})
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var resp settingsListResp
	assert.NoError(t, json.Unmarshal(getSettings(t, handler, settingsRobotID).Body.Bytes(), &resp))
	for _, key := range []string{BotSettingKeyDisplayEnabled, BotSettingKeyInteractionEnabled} {
		assert.Nil(t, findSetting(t, resp, key).Value,
			"%s was written despite the batch failing", key)
	}

	// The same batch with every item valid applies completely.
	w = putSettings(t, handler, settingsRobotID,
		map[string]any{"key": BotSettingKeyDisplayEnabled, "value": false},
		map[string]any{"key": BotSettingKeyInteractionEnabled, "value": false})
	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	assert.NoError(t, json.Unmarshal(getSettings(t, handler, settingsRobotID).Body.Bytes(), &resp))
	for _, key := range []string{BotSettingKeyDisplayEnabled, BotSettingKeyInteractionEnabled} {
		item := findSetting(t, resp, key)
		assert.NotNil(t, item.Value, "%s must be written when the whole batch is valid", key)
		assert.False(t, *item.Value)
	}
}

// TestWriteBotSettingOverrides_RollsBackOnMidBatchFailure exercises the
// transaction for real: the second item's key exceeds key_name's column width,
// so MySQL (strict mode) errors on it after the first has already been inserted
// inside the transaction. Without the rollback the first would survive.
//
// It calls the writer directly rather than going through HTTP because the
// registry whitelist makes an over-long key unreachable from the endpoint — which
// is the point: the only failure the HTTP path can produce trips before Begin().
func TestWriteBotSettingOverrides_RollsBackOnMidBatchFailure(t *testing.T) {
	_, ctx := setupBotSettings(t)

	overlong := strings.Repeat("k", 200) // key_name is VARCHAR(128)
	err := writeBotSettingOverrides(ctx, settingsRobotID, testutil.UID, []botSettingWritePlan{
		{key: BotSettingKeyDisplayEnabled, value: botSettingFalse},
		{key: overlong, value: botSettingFalse},
	})
	assert.Error(t, err, "an over-long key must fail the write")

	overrides, queryErr := queryBotSettingOverrides(ctx, settingsRobotID)
	assert.NoError(t, queryErr)
	assert.Empty(t, overrides,
		"the first item survived a failed batch — the transaction did not roll back")

	// The same batch without the poisoned item commits completely.
	assert.NoError(t, writeBotSettingOverrides(ctx, settingsRobotID, testutil.UID, []botSettingWritePlan{
		{key: BotSettingKeyDisplayEnabled, value: botSettingFalse},
		{key: BotSettingKeyInteractionEnabled, value: botSettingFalse},
	}))
	overrides, queryErr = queryBotSettingOverrides(ctx, settingsRobotID)
	assert.NoError(t, queryErr)
	assert.Len(t, overrides, 2)
}
