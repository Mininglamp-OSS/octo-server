package bot_api

// task bot-setting-owner-contract / P2-1 —— owner 目录与生产者清单的**跨端点**一致性。
//
// 为什么这条用例必须住在 bot_api 包：两个端点分属两个模块，而 modules/robot 的测试
// 二进制里根本没有 bot_api（依赖方向是 bot_api → robot），`/v1/bot/card/profile` 的
// 路由压根不存在。只断言单端点抓不到「两处对同一个 Bot 的同一能力给出相反答案」这件
// 事本身——而那正是本任务要修的缺陷形态，不是它的某个侧影。
//
// 断言的是**两端相等**，不是「等于 false」。后者把预期值写死，总闸语义一改就得跟着
// 改，且总闸开着时两端恰好相等、用例会在真正的矛盾场景之外空转。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/robot"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	"github.com/go-redis/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	parityBotID    = "bot_setting_parity"
	parityBotToken = "bf_setting_parity_token"
)

// settingsCatalogItem mirrors one row of GET /v1/robot/:id/settings. Declared
// here rather than shared with the robot package's copy on purpose: this test
// consumes the endpoint the way a client does, over the wire.
type settingsCatalogItem struct {
	Key       string `json:"key"`
	Value     *bool  `json:"value"`
	Effective bool   `json:"effective_value"`
	Source    string `json:"source"`
}

type settingsCatalogResp struct {
	List []settingsCatalogItem `json:"list"`
}

// profileConfigResp mirrors the `config` object of GET /v1/bot/card/profile.
type profileConfigResp struct {
	Config struct {
		CardEnabled        bool `json:"card_enabled"`
		DisplayEnabled     bool `json:"display_enabled"`
		InteractionEnabled bool `json:"interaction_enabled"`
		ReasoningEnabled   bool `json:"reasoning_enabled"`
	} `json:"config"`
}

// setupBotSettingParity wires one bot that is BOTH owned by the test user (so
// the owner catalog is reachable with a user session) and holds a bot token (so
// the producer manifest is reachable with bot auth). One bot, two identities —
// otherwise the two endpoints would be answering about different subjects and
// any agreement between them would be meaningless.
func setupBotSettingParity(t *testing.T) (http.Handler, *config.Context) {
	t.Helper()
	previousRegistry := cardtmpl.DefaultRegistry()
	cardtmpl.SetDefaultRegistry(testBotTemplateRegistry(t))
	t.Cleanup(func() { cardtmpl.SetDefaultRegistry(previousRegistry) })

	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	// The settings routes mount SharedUIDRateLimiter and its Redis bucket
	// outlives CleanAllTables, so without this a later case in the binary can
	// start tripping 429 and the failure reads as an assertion bug.
	rds := redis.NewClient(&redis.Options{
		Addr:     ctx.GetConfig().DB.RedisAddr,
		Password: ctx.GetConfig().DB.RedisPass,
	})
	defer rds.Close()
	if keys, err := rds.Keys("ratelimit:uid:*").Result(); err == nil && len(keys) > 0 {
		_ = rds.Del(keys...).Err()
	}

	_, err := ctx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, status, creator_uid, bot_token) VALUES (?, 1, ?, ?)",
		parityBotID, testutil.UID, parityBotToken,
	).Exec()
	require.NoError(t, err)
	return s.GetRoute(), ctx
}

func getOwnerCatalog(t *testing.T, handler http.Handler) settingsCatalogResp {
	t.Helper()
	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/robot/"+parityBotID+"/settings", nil)
	require.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "owner catalog: body=%s", w.Body.String())

	var resp settingsCatalogResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp
}

func getProducerConfig(t *testing.T, handler http.Handler) profileConfigResp {
	t.Helper()
	w := getCardProfile(t, handler, parityBotToken)
	require.Equal(t, http.StatusOK, w.Code, "producer manifest: body=%s", w.Body.String())

	var resp profileConfigResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp
}

func findCatalogItem(t *testing.T, resp settingsCatalogResp, key string) settingsCatalogItem {
	t.Helper()
	for _, item := range resp.List {
		if item.Key == key {
			return item
		}
	}
	t.Fatalf("key %s missing from the owner catalog", key)
	return settingsCatalogItem{}
}

// TestBotCardCapability_OwnerCatalogAgreesWithProducerManifest is the P2-1
// regression.
//
// Before the projection, listBotSettings returned each key's raw three-layer
// resolution while the manifest ran the same config through the master switch
// and the AllowsRaw* predicates. Two shapes of disagreement followed, and both
// are covered below:
//
//   - master switch off: the catalog reported display_enabled=true, the manifest
//     reported false, and every send 400'd. An owner saw "on" in the management
//     page and could not send a single card.
//   - display=false + interaction=true: the catalog reported
//     interaction_enabled=true while the send gate requires both, because
//     octo/v2 is a strict superset of octo/v1 and display is the floor.
//
// Known residual, deliberately not asserted away: the manifest ANDs
// reasoning_enabled with "does this deployment's template catalog advertise the
// reasoning template", which the owner catalog cannot see. That is a
// composition fact rather than a policy fact, and mirroring it would invert the
// module dependency (robot would have to know about bot_api's catalog). This
// test runs with the template advertised, i.e. the production shape.
func TestBotCardCapability_OwnerCatalogAgreesWithProducerManifest(t *testing.T) {
	handler, ctx := setupBotSettingParity(t)

	cases := []struct {
		name      string
		master    bool
		overrides map[string]string
	}{
		{name: "master off, no overrides", master: false},
		{name: "master on, no overrides", master: true},
		{
			// The floor case: display is not a peer of interaction, it is its
			// lower bound.
			name: "master on, display off but interaction on", master: true,
			overrides: map[string]string{
				robot.BotSettingKeyDisplayEnabled:     "0",
				robot.BotSettingKeyInteractionEnabled: "1",
			},
		},
		{
			// The worst case for the owner: everything explicitly switched on in
			// the store, and the deployment gate shut. Nothing may be advertised
			// as available on either side.
			name: "master off with every override on", master: false,
			overrides: map[string]string{
				robot.BotSettingKeyDisplayEnabled:     "1",
				robot.BotSettingKeyInteractionEnabled: "1",
				robot.BotSettingKeyReasoningEnabled:   "1",
			},
		},
		{
			name: "master on, reasoning off", master: true,
			overrides: map[string]string{robot.BotSettingKeyReasoningEnabled: "0"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ctx.DB().DeleteFrom("bot_setting").
				Where("robot_id=?", parityBotID).Exec()
			require.NoError(t, err)
			for key, value := range tc.overrides {
				_, err := ctx.DB().InsertBySql(
					"INSERT INTO bot_setting (robot_id, key_name, value, updated_by) VALUES (?, ?, ?, ?)",
					parityBotID, key, value, testutil.UID,
				).Exec()
				require.NoError(t, err)
			}
			master := ""
			if tc.master {
				master = "true"
			}
			t.Setenv(cardmsg.EnvEnabled, master)
			t.Setenv(cardmsg.EnvBotEnabled, master)

			catalog := getOwnerCatalog(t, handler)
			manifest := getProducerConfig(t, handler)

			for _, pair := range []struct {
				catalogKey string
				advertised bool
			}{
				{robot.BotSettingKeyCardEnabled, manifest.Config.CardEnabled},
				{robot.BotSettingKeyDisplayEnabled, manifest.Config.DisplayEnabled},
				{robot.BotSettingKeyInteractionEnabled, manifest.Config.InteractionEnabled},
				{robot.BotSettingKeyReasoningEnabled, manifest.Config.ReasoningEnabled},
			} {
				item := findCatalogItem(t, catalog, pair.catalogKey)
				assert.Equal(t, pair.advertised, item.Effective,
					"%s: owner catalog says effective_value=%v but the producer manifest says %v — "+
						"the owner's management page and the bot's capability discovery disagree "+
						"about the same capability of the same bot",
					pair.catalogKey, item.Effective, pair.advertised)
			}
		})
	}
}

// TestBotCardCapability_ParitySuiteIsNotVacuous guards the suite above against
// the way it could rot into passing for free: if every capability were false in
// every case, "the two agree" would hold no matter what either side computed.
//
// So this pins that the master-on baseline actually advertises something. It is
// the assertion that makes the parity cases load-bearing.
func TestBotCardCapability_ParitySuiteIsNotVacuous(t *testing.T) {
	handler, _ := setupBotSettingParity(t)
	t.Setenv(cardmsg.EnvEnabled, "true")
	t.Setenv(cardmsg.EnvBotEnabled, "true")

	manifest := getProducerConfig(t, handler)
	assert.True(t, manifest.Config.DisplayEnabled,
		"with the master switch on and no overrides, display must be advertised — "+
			"otherwise the parity cases compare false against false and prove nothing")

	catalog := getOwnerCatalog(t, handler)
	display := findCatalogItem(t, catalog, robot.BotSettingKeyDisplayEnabled)
	assert.True(t, display.Effective)
	// The projection must not leak into `value`: the owner UI tells "I explicitly
	// set this" apart from "nobody set it and the default is true" through this
	// field alone, and that distinction is what the restore-default control is
	// built on.
	assert.Nil(t, display.Value,
		"effective_value's projection polluted `value`; value must stay the explicit override")
}
