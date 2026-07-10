package bot_api

// card-message-interaction P2 D12：GET /v1/bot/card/profile 能力清单测试。
// 纯读端点，只需 MySQL（authBot 查 robot.bot_token）—— 无 WuKongIM 依赖。
// 「both-halves」用例的 send-reject 分支在 IM 派发前拒绝（send.go:97），同样无需 :5001。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/stretchr/testify/assert"
)

const (
	cpBotID    = "bot_card_profile"
	cpBotToken = "bf_card_profile_token"
)

func setupBotCardProfile(t *testing.T) (http.Handler, *config.Context) {
	t.Helper()
	s, ctx := testutil.NewTestServer()
	assert.NoError(t, testutil.CleanAllTables(ctx))
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, status, creator_uid, bot_token) VALUES (?, 1, ?, ?)",
		cpBotID, "owner_cp", cpBotToken,
	).Exec()
	assert.NoError(t, err)
	return s.GetRoute(), ctx
}

func getCardProfile(t *testing.T, handler http.Handler, token string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/bot/card/profile", nil)
	assert.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	handler.ServeHTTP(w, req)
	return w
}

// cardProfileManifest pins the D12 wire shape (field names decode-checked).
type cardProfileManifest struct {
	Enabled     bool     `json:"enabled"`
	CardVersion string   `json:"card_version"`
	Profiles    []string `json:"profiles"`
	Elements    []string `json:"elements"`
	Inputs      []string `json:"inputs"`
	Actions     []string `json:"actions"`
	Limits      struct {
		MaxPayloadBytes   int `json:"max_payload_bytes"`
		MaxNodes          int `json:"max_nodes"`
		MaxDepth          int `json:"max_depth"`
		MaxInputTextBytes int `json:"max_input_text_bytes"`
		MaxInputsBytes    int `json:"max_inputs_bytes"`
		MaxCopyTextBytes  int `json:"max_copy_text_bytes"`
	} `json:"limits"`
}

// TestBotCardProfile_ElementsInputsFromConstants（D12.2）：elements/inputs 深等 cardmsg
// 权威白名单 —— 供 J3 gate 按元素/输入粒度做前向兼容协商（即便 card_version 不变，也能
// 探测本部署是否接受 Input.Number/Date/Time 等新增元素，消除「版本号不动 → gate 无法
// 分辨新旧 1.5 部署」的错配）。
func TestBotCardProfile_ElementsInputsFromConstants(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	handler, _ := setupBotCardProfile(t)

	w := getCardProfile(t, handler, cpBotToken)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var m cardProfileManifest
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &m))
	assert.Equal(t, cardmsg.DisplayElements(), m.Elements)
	assert.Equal(t, cardmsg.InputElements(), m.Inputs)
	assert.Equal(t, cardmsg.DisplayActions(), m.Actions)
}

// TestBotCardProfile_ValuesFromConstants：清单每个值必须等于 pkg/cardmsg 常量
// （D12.2 单一权威，非重抄字面量）；profiles 深等 AcceptedProfiles()。
func TestBotCardProfile_ValuesFromConstants(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	handler, _ := setupBotCardProfile(t)

	w := getCardProfile(t, handler, cpBotToken)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var m cardProfileManifest
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &m))
	assert.True(t, m.Enabled)
	assert.Equal(t, cardmsg.CardVersion, m.CardVersion)
	assert.Equal(t, cardmsg.AcceptedProfiles(), m.Profiles)
	assert.Equal(t, cardmsg.MaxPayloadBytes, m.Limits.MaxPayloadBytes)
	assert.Equal(t, cardmsg.MaxNodes, m.Limits.MaxNodes)
	assert.Equal(t, cardmsg.MaxDepth, m.Limits.MaxDepth)
	assert.Equal(t, cardmsg.MaxInputTextBytes, m.Limits.MaxInputTextBytes)
	assert.Equal(t, cardmsg.MaxInputsBytes, m.Limits.MaxInputsBytes)
	assert.Equal(t, cardmsg.MaxCopyTextBytes, m.Limits.MaxCopyTextBytes)
}

// TestBotCardProfile_DisabledStillReturnsManifestAndSendRejects（D12.3）：rollout
// flag 关闭时——(半 1) 清单仍返 200 + enabled:false + 全清单（feature detection 正是
// 目的）；(半 2) 同一 cardmsg.Enabled() 门禁在 send 路径仍拒绝卡片。两半同测，锁死
// 「清单只读、send 才拒」不漂移。
func TestBotCardProfile_DisabledStillReturnsManifestAndSendRejects(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "") // 强制关闭，无视环境既有值
	handler, _ := setupBotCardProfile(t)

	// 半 1：清单读 200 + enabled:false + 全清单仍在。
	w := getCardProfile(t, handler, cpBotToken)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var m cardProfileManifest
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &m))
	assert.False(t, m.Enabled, "rollout flag off → enabled:false")
	assert.Equal(t, cardmsg.CardVersion, m.CardVersion, "关闭时仍返完整清单")
	assert.Equal(t, cardmsg.AcceptedProfiles(), m.Profiles)
	assert.Equal(t, cardmsg.MaxPayloadBytes, m.Limits.MaxPayloadBytes)

	// 半 2：send 路径对卡片仍拒绝（同一 cardmsg.Enabled() 门禁，send.go:97 —— 在
	// IM 派发前拒绝，无需 WuKongIM）。
	body := map[string]interface{}{
		"channel_id":   testutil.UID,
		"channel_type": common.ChannelTypePerson.Uint8(),
		"payload": map[string]interface{}{
			"type":         cardmsg.InteractiveCard.Int(),
			"card_version": cardmsg.CardVersion,
			"profile":      cardmsg.ProfileV1,
			"card":         map[string]interface{}{"type": "AdaptiveCard", "version": "1.5"},
		},
	}
	ws := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/bot/sendMessage", bytes.NewReader([]byte(util.ToJson(body))))
	req.Header.Set("Authorization", "Bearer "+cpBotToken)
	handler.ServeHTTP(ws, req)
	assert.Equal(t, http.StatusBadRequest, ws.Code, ws.Body.String())
	assert.Contains(t, ws.Body.String(), "Card messages are not enabled on this server.")
}

// TestBotCardProfile_Unauthenticated：缺 / 错 bot-token 走既有 authBot 拒绝（非 200）。
// D12 不引入新鉴权代码。
func TestBotCardProfile_Unauthenticated(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	handler, _ := setupBotCardProfile(t)

	w := getCardProfile(t, handler, "")
	assert.NotEqual(t, http.StatusOK, w.Code, "缺 token 必须被 authBot 拒绝; body=%s", w.Body.String())

	w2 := getCardProfile(t, handler, "bf_not_a_real_token")
	assert.NotEqual(t, http.StatusOK, w2.Code, "非法 token 必须被拒绝; body=%s", w2.Body.String())
}

// TestBotCardProfile_AdditiveContractFieldSet（D12.4）：pin 死顶层 + limits 字段集，
// 任何改名/删除都会让本测试失败（additive-only：只许新增字段）。
func TestBotCardProfile_AdditiveContractFieldSet(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	handler, _ := setupBotCardProfile(t)

	w := getCardProfile(t, handler, cpBotToken)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var raw map[string]json.RawMessage
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	gotTop := make([]string, 0, len(raw))
	for k := range raw {
		gotTop = append(gotTop, k)
	}
	assert.ElementsMatch(t, []string{"enabled", "card_version", "profiles", "limits", "elements", "inputs", "actions"}, gotTop,
		"D12 additive-only：顶层字段集冻结（新增新字段，绝不改名/删除）")

	var limits map[string]json.RawMessage
	assert.NoError(t, json.Unmarshal(raw["limits"], &limits))
	gotLimits := make([]string, 0, len(limits))
	for k := range limits {
		gotLimits = append(gotLimits, k)
	}
	assert.ElementsMatch(t, []string{
		"max_payload_bytes", "max_nodes", "max_depth", "max_input_text_bytes", "max_inputs_bytes",
		"max_copy_text_bytes",
	}, gotLimits, "D12 additive-only：limits 字段集冻结")
}
