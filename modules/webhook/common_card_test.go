package webhook

// card-message-protocol P1：离线推送的卡片分支 —— Decision 8（推送正文 = 权威
// plain）+ Decision 2 residual-risk（sender 非 bot/webhook 身份 → [卡片] 遮蔽，
// round-3 P1-2 统一执法点）。

import (
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/stretchr/testify/assert"
)

func TestCardSenderTrusted(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()

	// iwh_ 合成身份 → 可信（无需查库）
	assert.True(t, cardSenderTrusted(ctx, "iwh_abc123"))

	// robot 表在册（status=1）→ 可信
	_, err := ctx.DB().InsertBySql("insert into robot(robot_id,status) values(?,1)", "bot_push_1").Exec()
	assert.NoError(t, err)
	assert.True(t, cardSenderTrusted(ctx, "bot_push_1"))

	// 普通用户 → 不可信（直连长连接可伪造 type-17,plain 攻击者可控）
	assert.False(t, cardSenderTrusted(ctx, "human_9527"))
}

// 验收：推送 alert —— bot sender 取权威 plain(绝不出现原始卡 JSON);
// 非可信 sender 遮蔽为 [卡片]。
func TestGetMessageAlertCard(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	ctx.GetConfig().Push.ContentDetailOn = true

	_, err := ctx.DB().InsertBySql("insert into robot(robot_id,status) values(?,1)", "bot_push_2").Exec()
	assert.NoError(t, err)

	payload := []byte(`{"type":17,"card":{"body":[{"type":"TextBlock","text":"内部字段"}]},"plain":"审批单 #42:待审批","card_version":"1.5","profile":"octo/v1"}`)
	var payloadMap map[string]interface{}
	assert.NoError(t, json.Unmarshal(payload, &payloadMap))
	// json.Number 语义与生产解码路径一致
	payloadMap["type"] = json.Number("17")

	toUser := &user.Resp{MsgShowDetail: 1}
	mk := func(fromUID string) msgOfflineNotify {
		m := msgOfflineNotify{}
		m.FromUID = fromUID
		m.Payload = payload
		m.PayloadMap = payloadMap
		return m
	}

	alert, err := getMessageAlert(mk("bot_push_2"), toUser, ctx)
	assert.NoError(t, err)
	assert.Equal(t, "审批单 #42:待审批", alert, "bot sender 推送正文 = 权威 plain")
	assert.NotContains(t, alert, "{", "APNs/FCM alert 不得出现原始卡 JSON")

	alert, err = getMessageAlert(mk("human_9527"), toUser, ctx)
	assert.NoError(t, err)
	assert.Equal(t, cardmsg.PlaceholderCard, alert, "非可信 sender 必须遮蔽为 [卡片]")
}
