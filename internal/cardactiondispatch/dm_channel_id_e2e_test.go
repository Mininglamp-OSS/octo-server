package cardactiondispatch_test

// DM 卡片回调的频道语义契约（Bot 发卡 → 用户点击 → 事件里的 channel_id）。
//
// 本文件**有意不挂 `//go:build integration`**。card/action 的既有 e2e 全挂着那个
// tag —— 本包的 orchestration_integration_test.go、modules/message 的
// api_card_action_test.go 都是 —— 而 CI 的两条 lane 跑的都是默认构建
// （.github/workflows/ci.yml 与 ci/run-e2e-shard.sh 里 issue #557 的那段注释写明了
// 原因），那套用例从不执行。DM channel_id 是插件侧安全校验的判定依据，回归即「所有 DM
// 交互卡按钮无响应」，必须落在每次 CI 都会跑的 lane 上。本包在 ci/e2e-package-weights.tsv
// 里，e2e lane 会给它 MySQL/Redis/WuKongIM。
//
// 放在本包而不是 modules/message，有两个硬理由：
//   - modules/message 的默认构建混着「手建最小表」的单元测试，再插一个跑 sql-migrate
//     的用例会撞 `Error 1050 Table already exists`（这正是那边整套 card e2e 被打上
//     integration tag 的起因）。本包默认构建没有手建表的邻居。
//   - 外部测试包可以正常 `_ import` modules/app_bot —— sender 身份解析要查 app_bot 表
//     （modules/botidentity/resolver.go 一条 SQL 同时 EXISTS robot 和 app_bot）。在
//     modules/message 里导入它会形成 app_bot → bot_api → messages_search → message 的
//     导入环，只能手抄 DDL；这里能直接用模块自己的迁移。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/internal/cardactiondispatch"
	_ "github.com/Mininglamp-OSS/octo-server/modules/app_bot"
	_ "github.com/Mininglamp-OSS/octo-server/modules/base"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	_ "github.com/Mininglamp-OSS/octo-server/modules/message"
	_ "github.com/Mininglamp-OSS/octo-server/modules/robot"
	_ "github.com/Mininglamp-OSS/octo-server/modules/thread"
	_ "github.com/Mininglamp-OSS/octo-server/modules/webhook"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/go-redis/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	dmPeerBotUID   = "bot_dm_peer_1"
	dmPeerActionID = "approve_btn"
	dmPeerGroupNo  = "g_dm_peer_card"

	dmPeerDMMessageID    int64 = 9601
	dmPeerGroupMessageID int64 = 9602

	// 内部路由那条用例自己的一套身份：注册的 (sender_uid, owner, action_type) 三元组
	// 决定这次点击走可靠队列而不是 bot 拉取队列。刻意与上面的 bot 分开 —— 注册过路由的
	// sender 提交未知 owner/action 会被 fail-closed 拒绝，共用会污染 DM/群两个子用例。
	dmPeerRouteBotUID           = "bot_dm_peer_route"
	dmPeerRouteActionID         = "approve"
	dmPeerRouteOwner            = "docs"
	dmPeerRouteActionType       = "access_request.decision"
	dmPeerRouteMessageID  int64 = 9603

	dmPeerQueuePrefix = "test:cardaction:dm_peer_channel_id"
	dmPeerRouteSecret = "0123456789abcdef0123456789abcdef"
)

// dmPeerQueueSuffixes 是 RedisQueue 的固定 key 后缀（见 queue.go 的 queueKeys 构造）。
// 前缀本用例独占，所以可以逐个精确删除，不必扫 keyspace。
var dmPeerQueueSuffixes = []string{
	"ready", "leased", "payload", "attempts", "tokens",
	"dlq", "dlq_payload", "dlq_meta", "route_missing_since",
}

// dmPeerRoutedCardEnvelope 的 Submit.data 带 owner/action_type，用来命中注册的路由。
func dmPeerRoutedCardEnvelope(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]interface{}{
		"type":         cardmsg.InteractiveCard.Int(),
		"card_version": cardmsg.CardVersion,
		"profile":      cardmsg.ProfileV2,
		"plain":        "文档访问申请",
		"space_id":     "space-dm-peer",
		"card": map[string]interface{}{
			"type": "AdaptiveCard", "version": cardmsg.CardVersion,
			"body": []interface{}{map[string]interface{}{"type": "TextBlock", "text": "文档访问申请"}},
			"actions": []interface{}{map[string]interface{}{
				"type": "Action.Submit", "id": dmPeerRouteActionID, "title": dmPeerRouteActionID,
				"data": map[string]interface{}{
					"owner": dmPeerRouteOwner, "action_type": dmPeerRouteActionType,
					"decision": "approve", "doc_id": "doc-1", "request_id": "request-1",
				},
			}},
		},
	})
	require.NoError(t, err)
	return raw
}

// dmPeerCardEnvelope 一张最小 octo/v2 交互卡：单个 Action.Submit，无 Input。
func dmPeerCardEnvelope(t *testing.T) []byte {
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
				"type": "Action.Submit", "id": dmPeerActionID, "title": dmPeerActionID,
			}},
		},
	})
	require.NoError(t, err)
	return raw
}

// dmPeerSeedCardMessage 直接落一行 type-17 消息。card/action 只读这行，不经 IM，
// 所以无需 SendMessageWithResult 那趟往返（分表算法与 modules/message 的 db.getTable 一致）。
func dmPeerSeedCardMessage(t *testing.T, ctx *config.Context, messageID int64, fromUID, channelID string, channelType uint8, payload []byte) {
	t.Helper()
	table := "message"
	if count := ctx.GetConfig().TablePartitionConfig.MessageTableCount; count > 0 {
		if index := crc32.ChecksumIEEE([]byte(channelID)) % uint32(count); index > 0 {
			table = fmt.Sprintf("message%d", index)
		}
	}
	_, err := ctx.DB().InsertBySql(fmt.Sprintf(
		"INSERT INTO `%s` (message_id,message_seq,client_msg_no,from_uid,channel_id,channel_type,timestamp,payload,is_deleted) VALUES (?,?,?,?,?,?,?,?,0)", table),
		messageID, 1, fmt.Sprintf("dm-peer-%d", messageID), fromUID, channelID, channelType, time.Now().Unix(), payload,
	).Exec()
	require.NoError(t, err)
}

// dmPeerResetRedis 只删本用例自己那几个确切的 key。
//
// 不用 `KEYS <pattern>` + 全量 DEL：Redis keyspace 跨包共享，testutil.UID 也跨包通用，
// 并行跑 `go test ./...` 时全量删会清掉别的包正在用的限流桶和事件队列，KEYS 本身还是
// O(keyspace) 的阻塞命令。同理绝不碰 common.RobotEventSeqKey —— 那是进程级单调分配器，
// 别的队列还有成员时把它清零会造出重复 score。口径同
// modules/channel/channel_get_authz_test.go 的 resetChannelUIDRateLimit。
func dmPeerResetRedis(t *testing.T, rds *redis.Client) {
	t.Helper()
	keys := []string{
		"ratelimit:uid:" + testutil.UID,
		"robotEvent:" + dmPeerBotUID,
		"robotEvent:" + dmPeerRouteBotUID,
		fmt.Sprintf("cardaction:%d:%s:%s", dmPeerRouteMessageID, dmPeerRouteActionID, testutil.UID),
	}
	for _, messageID := range []int64{dmPeerDMMessageID, dmPeerGroupMessageID} {
		keys = append(keys, fmt.Sprintf("cardaction:%d:%s:%s", messageID, dmPeerActionID, testutil.UID))
	}
	// 上一轮跑剩的队列残留会让 Claim 认领到别的事件。
	for _, suffix := range dmPeerQueueSuffixes {
		keys = append(keys, dmPeerQueuePrefix+":{queue}:"+suffix)
	}
	require.NoError(t, rds.Del(keys...).Err())
}

// TestCardActionDMChannelIDIsPeerUID 冻结 DM 回调的频道语义：
//
//	Bot 发卡用 POST /v1/bot/sendMessage {"channel_id": "<用户 UID>"}；
//	用户点击用 POST /v1/message/card/action {"channel_id": "<bot UID>"}（操作者视角）；
//	事件里的 channel_id 必须回到**消费方视角** = 对端用户 UID = operator_uid。
//
// 回显请求值会把 bot 自己的 UID 当会话标识发回给 bot，按 channel_id 认卡的消费方
// （OpenClaw 插件按注册发卡时的 channelId 校验）就把每一次 DM 点击判为 mismatch 丢弃，
// 表现为按钮无响应。Group / CommunityTopic 的 channel_id 指频道自身、与视角无关，
// 保持原样回显。
//
// event_data.channel_id 同时是内部 HMAC 回调路由那条出口的来源：
// cardactiondispatch.Event.ChannelID 由它派生（见 modules/message/api_card_action.go
// 的 stringEventField(eventData, "channel_id")），所以这里断言的是两条投递路径共用的
// 同一个值。
func TestCardActionDMChannelIDIsPeerUID(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	// modules/common 的 Route 在 module.Setup 里就要落加密私钥，缺 key 直接 panic。
	// CI 在 job env 里给了同一个值；这里显式设置让本地也能直接跑。
	t.Setenv("OCTO_MASTER_KEY", dmPeerRouteSecret)
	t.Setenv("OCTO_DM_PEER_CARD_ACTION_SECRET", dmPeerRouteSecret)
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()

	rds := redis.NewClient(&redis.Options{
		Addr:     ctx.GetConfig().DB.RedisAddr,
		Password: ctx.GetConfig().DB.RedisPass,
	})
	defer func() { _ = rds.Close() }()
	dmPeerResetRedis(t, rds)

	for _, robotID := range []string{dmPeerBotUID, dmPeerRouteBotUID} {
		_, err := ctx.DB().InsertBySql("INSERT IGNORE INTO robot(robot_id,status) VALUES(?,1)", robotID).Exec()
		require.NoError(t, err)
	}

	// 注册一条内部 callback 路由，让最后那个子用例走可靠队列而不是 bot 拉取队列。
	//
	// Install 是按 ctx 存的，而 register.GetModules 用 sync.Once 把模块绑在**进程里第一次**
	// NewTestServer 的那个 ctx 上。本包默认构建里这是唯一一个 NewTestServer 用例（其余
	// e2e 都挂着 //go:build integration），所以这次 Install 一定落到 message 模块看得见的
	// 那个 ctx。⚠️ 将来若给本包默认构建再加一个 NewTestServer 用例，两者会争这个 sync.Once，
	// 到时得把服务安装挪到共享的 TestMain 里。
	registry, err := cardactiondispatch.NewRegistry([]cardactiondispatch.RouteSpec{{
		SenderUID: dmPeerRouteBotUID, Owner: dmPeerRouteOwner, ActionType: dmPeerRouteActionType,
		URL: "https://internal.invalid/v1/card-actions/decide", SecretEnv: "OCTO_DM_PEER_CARD_ACTION_SECRET",
	}}, func(string) string { return dmPeerRouteSecret })
	require.NoError(t, err)
	queue, err := cardactiondispatch.NewRedisQueue(rds, cardactiondispatch.QueueConfig{
		Prefix:       dmPeerQueuePrefix,
		LiveTTL:      ctx.GetConfig().Robot.MessageExpire,
		DLQRetention: 30 * 24 * time.Hour,
	})
	require.NoError(t, err)
	service, err := cardactiondispatch.NewService(registry, queue, ctx)
	require.NoError(t, err)
	require.NoError(t, cardactiondispatch.Install(ctx, service))

	// 两个助手都显式收 *testing.T：闭包捕获外层 t、却在 t.Run 子用例里调用时，require
	// 失败会对**父** t 调 FailNow → 在子用例 goroutine 上 runtime.Goexit，子用例随即以
	// "test executed panic(nil) or runtime.Goexit" 收场，把断言信息换成一条无从下手的
	// panic —— 偏偏就发生在本文件真正抓到回归的那一刻。
	click := func(t *testing.T, body map[string]interface{}) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req, reqErr := http.NewRequest(http.MethodPost, "/v1/message/card/action",
			bytes.NewReader([]byte(util.ToJson(body))))
		require.NoError(t, reqErr)
		req.Header.Set("token", testutil.Token)
		s.GetRoute().ServeHTTP(w, req)
		return w
	}
	// bot 事件队列里最新一条 card_action 的 event_data —— bot 侧真正看到的形状。
	latestEventData := func(t *testing.T) map[string]interface{} {
		t.Helper()
		entries, rangeErr := rds.ZRange("robotEvent:"+dmPeerBotUID, -1, -1).Result()
		require.NoError(t, rangeErr)
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
		dmChannel := common.GetFakeChannelIDWith(testutil.UID, dmPeerBotUID)
		dmPeerSeedCardMessage(t, ctx, dmPeerDMMessageID, dmPeerBotUID, dmChannel,
			common.ChannelTypePerson.Uint8(), dmPeerCardEnvelope(t))

		w := click(t, map[string]interface{}{
			"message_id": fmt.Sprintf("%d", dmPeerDMMessageID),
			// 操作者视角：对端 = bot 自己。
			"channel_id":   dmPeerBotUID,
			"channel_type": common.ChannelTypePerson.Uint8(),
			"action_id":    dmPeerActionID, "client_token": "tok-dm-peer",
		})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		event := latestEventData(t)
		assert.Equal(t, testutil.UID, event["channel_id"],
			"DM channel_id 必须是对端用户 UID，而不是卡片 sender %s 自己", dmPeerBotUID)
		assert.Equal(t, event["operator_uid"], event["channel_id"], "本例里对端就是操作者")
	})

	t.Run("group echoes the channel itself", func(t *testing.T) {
		require.NoError(t, rds.Del("robotEvent:"+dmPeerBotUID).Err())
		_, insErr := ctx.DB().InsertBySql(
			"INSERT INTO `group`(group_no, name, status, space_id) VALUES(?, 'gp', ?, '')",
			dmPeerGroupNo, group.GroupStatusNormal).Exec()
		require.NoError(t, insErr)
		_, insErr = ctx.DB().InsertBySql("INSERT INTO group_member(group_no,uid) VALUES(?,?)",
			dmPeerGroupNo, testutil.UID).Exec()
		require.NoError(t, insErr)
		dmPeerSeedCardMessage(t, ctx, dmPeerGroupMessageID, dmPeerBotUID, dmPeerGroupNo,
			common.ChannelTypeGroup.Uint8(), dmPeerCardEnvelope(t))

		w := click(t, map[string]interface{}{
			"message_id":   fmt.Sprintf("%d", dmPeerGroupMessageID),
			"channel_id":   dmPeerGroupNo,
			"channel_type": common.ChannelTypeGroup.Uint8(),
			"action_id":    dmPeerActionID, "client_token": "tok-group-peer",
		})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		event := latestEventData(t)
		assert.Equal(t, dmPeerGroupNo, event["channel_id"], "群频道语义不变")
		assert.Equal(t, testutil.UID, event["operator_uid"])
	})

	// 内部 HMAC 回调那条出口。命中已注册路由的点击不进 bot 拉取队列，而是进
	// cardactiondispatch 的可靠队列，legacy DecisionRequest.channel_id 与
	// octo-card@1.0 的 card.channel_id 都由 Event.ChannelID 派生。这里断的是**真正
	// 入队**的那个 Event —— 构造一个已经正确的 Event 字面量再断言字段被拷贝，是测不
	// 到 ingress 的：把 api_card_action.go 的赋值改回 req.ChannelID，那种写法照样绿。
	t.Run("internal dispatch route carries the peer uid", func(t *testing.T) {
		dmChannel := common.GetFakeChannelIDWith(testutil.UID, dmPeerRouteBotUID)
		dmPeerSeedCardMessage(t, ctx, dmPeerRouteMessageID, dmPeerRouteBotUID, dmChannel,
			common.ChannelTypePerson.Uint8(), dmPeerRoutedCardEnvelope(t))

		w := click(t, map[string]interface{}{
			"message_id": fmt.Sprintf("%d", dmPeerRouteMessageID),
			// 同样是操作者视角：对端 = bot 自己。
			"channel_id":   dmPeerRouteBotUID,
			"channel_type": common.ChannelTypePerson.Uint8(),
			"action_id":    dmPeerRouteActionID, "client_token": "tok-dm-route",
		})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		// 命中内部路由就不该同时进 bot 队列，否则同一次点击会被消费两遍。
		botDepth, depthErr := rds.ZCard("robotEvent:" + dmPeerRouteBotUID).Result()
		require.NoError(t, depthErr)
		assert.Zero(t, botDepth, "命中内部路由的点击不应进 bot 事件队列")

		lease, claimErr := queue.Claim(time.Now().Add(time.Second), time.Minute)
		require.NoError(t, claimErr)
		require.NotNil(t, lease, "内部队列应恰好收到这次点击")
		assert.Equal(t, dmPeerRouteBotUID, lease.Event.SenderUID)
		assert.Equal(t, testutil.UID, lease.Event.ChannelID,
			"内部回调的 DM channel_id 同样是对端用户 UID，而不是 sender %s", dmPeerRouteBotUID)
		assert.Equal(t, testutil.UID, lease.Event.OperatorUID)
		// 两种 callback 线格式都由 Event.ChannelID 派生。
		assert.Equal(t, testutil.UID, cardactiondispatch.DecisionRequestFromEvent(lease.Event).ChannelID,
			"legacy DecisionRequest.channel_id")

		_, ackErr := queue.Ack(lease.Event.EventID, lease.Token)
		require.NoError(t, ackErr)
	})
}
