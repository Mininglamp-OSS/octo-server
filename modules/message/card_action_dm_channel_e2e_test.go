package message

// DM 卡片回调的频道语义契约（Bot 发卡 → 用户点击 → 事件里的 channel_id）。
//
// 本文件**有意不挂 `//go:build integration`**：本包的 card/action 既有 e2e
// （api_card_action_test.go）全部挂 integration tag，而 CI 的两条 lane 都跑默认
// 构建（见 .github/workflows/ci.yml 与 ci/run-e2e-shard.sh 的 issue #557 注释），
// 那套用例从不执行。DM channel_id 语义是插件侧安全校验的判定依据，回归即「所有 DM
// 交互卡按钮无响应」，必须落在每次 CI 都会跑的 lane 上 —— 与
// issue557_creator_thread_e2e_test.go 同样的取舍。
//
// card/action 全链路不触碰 WuKongIM（只读 message/message_extra + Redis），故这里
// 直接用 testutil.NewTestServer（与本包 api_message_get_test.go 等默认构建用例同
// 口径），无需隔离库或 IM 探活。
//
// 助手函数一律带 dmPeer 前缀：api_card_action_test.go 里有同义的 seedCardBot /
// seedCardMessage，带 -tags integration 时两个文件会同时编译，重名即冲突。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/go-redis/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const dmPeerCardBotUID = "bot_dm_peer_1"

// dmPeerCardEnvelope 一张最小 octo/v2 交互卡：单个 Action.Submit，无 Input。
func dmPeerCardEnvelope(t *testing.T, actionID string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]interface{}{
		"type":         cardmsg.InteractiveCard.Int(),
		"card_version": cardmsg.CardVersion,
		"profile":      cardmsg.ProfileV2,
		"plain":        "审批单",
		"card": map[string]interface{}{
			"type": "AdaptiveCard", "version": cardmsg.CardVersion,
			"body": []interface{}{map[string]interface{}{"type": "TextBlock", "text": "审批单"}},
			"actions": []interface{}{map[string]interface{}{
				"type": "Action.Submit", "id": actionID, "title": actionID,
			}},
		},
	})
	require.NoError(t, err)
	return raw
}

func dmPeerCardResetRedis(t *testing.T, ctx *config.Context) *redis.Client {
	t.Helper()
	rds := redis.NewClient(&redis.Options{
		Addr:     ctx.GetConfig().DB.RedisAddr,
		Password: ctx.GetConfig().DB.RedisPass,
	})
	t.Cleanup(func() { _ = rds.Close() })
	// UID 限流桶与幂等 claim 都跨用例存活在 Redis 里，CleanAllTables 不清。
	for _, pattern := range []string{"ratelimit:uid:*", "cardaction:*", "robotEvent:*", common.RobotEventSeqKey + "*"} {
		if keys, err := rds.Keys(pattern).Result(); err == nil && len(keys) > 0 {
			_ = rds.Del(keys...).Err()
		}
	}
	return rds
}

// dmPeerCardEnsureAppBotTable 建出 card/action 的 sender 身份解析所依赖的 app_bot 表。
//
// botidentity.Resolver 用一条 SQL 同时 EXISTS 查 robot 和 app_bot（见
// modules/botidentity/resolver.go），表缺失即整条查询报错、端点 fail-closed 回
// "Failed to query message data."。而 app_bot 模块的迁移只在该模块被链接进测试
// 二进制时才跑 —— 本包默认构建没有它（`_ "modules/app_bot"` 会形成
// app_bot → bot_api → messages_search → message 的导入环），所以这里手建。
//
// 语句逐字取自 modules/app_bot/sql/20260505000001_app_bot_legacy01.sql（本身就带
// IF NOT EXISTS）；解析器只读 uid/status/scope/space_id 四列。
func dmPeerCardEnsureAppBotTable(t *testing.T, ctx *config.Context) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(`
		CREATE TABLE IF NOT EXISTS app_bot (
		  id           VARCHAR(40) PRIMARY KEY,
		  uid          VARCHAR(40) UNIQUE NOT NULL,
		  display_name VARCHAR(100) NOT NULL,
		  description  VARCHAR(500) DEFAULT '',
		  avatar       VARCHAR(200) DEFAULT '',
		  scope        VARCHAR(20) NOT NULL DEFAULT 'platform' COMMENT 'platform or space',
		  space_id     VARCHAR(40) DEFAULT NULL,
		  status       TINYINT NOT NULL DEFAULT 0 COMMENT '0=draft 1=published 2=unpublished',
		  token        VARCHAR(100) UNIQUE NOT NULL,
		  created_by   VARCHAR(40) NOT NULL,
		  created_at   DATETIME NOT NULL DEFAULT NOW(),
		  updated_at   DATETIME NOT NULL DEFAULT NOW() ON UPDATE NOW(),
		  INDEX idx_scope_status (scope, status),
		  INDEX idx_space_status (space_id, status)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci`).Exec()
	require.NoError(t, err)
}

func dmPeerCardSeedBot(t *testing.T, ctx *config.Context, robotID string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql("insert into robot(robot_id,status) values(?,1)", robotID).Exec()
	require.NoError(t, err)
}

func dmPeerCardSeedMessage(t *testing.T, ctx *config.Context, messageID int64, fromUID, channelID string, channelType uint8, payload []byte) {
	t.Helper()
	require.NoError(t, NewDB(ctx).insertMessage(&messageModel{
		MessageID:   messageID,
		MessageSeq:  1,
		ClientMsgNo: fmt.Sprintf("cmn-dm-peer-%d", messageID),
		FromUID:     fromUID,
		ChannelID:   channelID,
		ChannelType: channelType,
		Timestamp:   time.Now().Unix(),
		Payload:     payload,
	}))
}

// TestCardActionDMChannelIDIsPeerUID 冻结 DM 回调的频道语义：
//
//	Bot 发卡用 POST /v1/bot/sendMessage {"channel_id": "<用户 UID>"}；
//	用户点击用 POST /v1/message/card/action {"channel_id": "<bot UID>"}（操作者视角）；
//	事件里的 channel_id 必须回到**消费方视角** = 对端用户 UID = operator_uid。
//
// 回显请求值会把 bot 自己的 UID 当会话标识发回给 bot，按 channel_id 认卡的消费方
// （OpenClaw 插件按注册发卡时的 channelId 校验）就把每一次 DM 点击判为 mismatch
// 丢弃，表现为按钮无响应。Group / CommunityTopic 的 channel_id 指频道自身、与视角
// 无关，保持原样回显。
func TestCardActionDMChannelIDIsPeerUID(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	// modules/common 的 Route 在 module.Setup 里就要落加密私钥，缺 key 直接 panic。
	// CI 在 job env 里给了同一个值；这里显式设置让本地 `go test ./modules/message/`
	// 也能直接跑（t.Setenv 用例结束即还原）。
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	rds := dmPeerCardResetRedis(t, ctx)

	dmPeerCardEnsureAppBotTable(t, ctx)
	dmPeerCardSeedBot(t, ctx, dmPeerCardBotUID)

	click := func(body map[string]interface{}) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req, err := http.NewRequest(http.MethodPost, "/v1/message/card/action", bytes.NewReader([]byte(util.ToJson(body))))
		require.NoError(t, err)
		req.Header.Set("token", testutil.Token)
		s.GetRoute().ServeHTTP(w, req)
		return w
	}
	// bot 事件队列里最新一条 card_action 的 event_data —— 即 bot 侧真正看到的形状。
	latestEventData := func() map[string]interface{} {
		t.Helper()
		entries, err := rds.ZRange("robotEvent:"+dmPeerCardBotUID, -1, -1).Result()
		require.NoError(t, err)
		require.Len(t, entries, 1)
		var ev struct {
			EventType string                 `json:"event_type"`
			EventData map[string]interface{} `json:"event_data"`
		}
		require.NoError(t, json.Unmarshal([]byte(entries[0]), &ev))
		require.Equal(t, cardmsg.EventTypeCardAction, ev.EventType)
		return ev.EventData
	}

	t.Run("dm reports the peer user uid", func(t *testing.T) {
		// 存储行的 fake 频道就是「操作者 + bot」这一对。
		dmChannel := common.GetFakeChannelIDWith(testutil.UID, dmPeerCardBotUID)
		dmPeerCardSeedMessage(t, ctx, 9601, dmPeerCardBotUID, dmChannel, common.ChannelTypePerson.Uint8(),
			dmPeerCardEnvelope(t, "approve_btn"))

		w := click(map[string]interface{}{
			"message_id": "9601", "channel_id": dmPeerCardBotUID, // 操作者视角:对端 = bot
			"channel_type": common.ChannelTypePerson.Uint8(),
			"action_id":    "approve_btn", "client_token": "tok-dm-peer",
		})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		event := latestEventData()
		assert.Equal(t, testutil.UID, event["channel_id"], "DM channel_id 必须是对端用户 UID")
		assert.Equal(t, event["operator_uid"], event["channel_id"], "DM 里对端 == 操作者")
		assert.NotEqual(t, dmPeerCardBotUID, event["channel_id"], "绝不能回 bot 自身 UID")
	})

	t.Run("group echoes the channel itself", func(t *testing.T) {
		_, err := rds.Del("robotEvent:" + dmPeerCardBotUID).Result()
		require.NoError(t, err)
		const groupNo = "g_dm_peer_card"
		_, err = ctx.DB().InsertBySql("INSERT INTO `group`(group_no, name, status, space_id) VALUES(?, 'gp', ?, '')",
			groupNo, group.GroupStatusNormal).Exec()
		require.NoError(t, err)
		_, err = ctx.DB().InsertBySql("insert into group_member(group_no,uid) values(?,?)", groupNo, testutil.UID).Exec()
		require.NoError(t, err)
		dmPeerCardSeedMessage(t, ctx, 9602, dmPeerCardBotUID, groupNo, common.ChannelTypeGroup.Uint8(),
			dmPeerCardEnvelope(t, "approve_btn"))

		w := click(map[string]interface{}{
			"message_id": "9602", "channel_id": groupNo,
			"channel_type": common.ChannelTypeGroup.Uint8(),
			"action_id":    "approve_btn", "client_token": "tok-group-peer",
		})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		event := latestEventData()
		assert.Equal(t, groupNo, event["channel_id"], "群频道语义不变")
		assert.Equal(t, testutil.UID, event["operator_uid"])
	})
}
