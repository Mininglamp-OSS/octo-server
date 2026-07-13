package webhook

// card-message-protocol P1：离线推送的卡片分支 —— Decision 8（推送正文 = 权威
// plain）+ Decision 2 residual-risk（sender 非 bot/webhook 身份 → [卡片] 遮蔽，
// round-3 P1-2 统一执法点）。

import (
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/stretchr/testify/assert"
)

func ensurePushAppBotTable(t *testing.T, ctx *config.Context) {
	t.Helper()
	_, err := ctx.DB().Exec(`CREATE TABLE IF NOT EXISTS app_bot (
		id VARCHAR(40) PRIMARY KEY,
		uid VARCHAR(40) UNIQUE NOT NULL,
		display_name VARCHAR(100) NOT NULL,
		description VARCHAR(500) DEFAULT '',
		avatar VARCHAR(200) DEFAULT '',
		scope VARCHAR(20) NOT NULL DEFAULT 'platform',
		space_id VARCHAR(40) DEFAULT NULL,
		status TINYINT NOT NULL DEFAULT 0,
		token VARCHAR(100) UNIQUE NOT NULL,
		created_by VARCHAR(40) NOT NULL,
		created_at DATETIME NOT NULL DEFAULT NOW(),
		updated_at DATETIME NOT NULL DEFAULT NOW() ON UPDATE NOW()
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci`)
	assert.NoError(t, err)
}

// 验收(PR#543 review B1):type-17 必须通过 containSupportType 门,否则离线推送
// 在 getMessageAlert 之前就把卡片当「不支持类型」丢弃 —— 下面的 alert 遮蔽分支
// 成死代码。本测试经真实门(w.supportTypes 由 New→getSupportTypes 装配),而非
// 直调 getMessageAlert;修复前 InteractiveCard 不在支持集,本断言为 false。
func TestCardTypeReachesPushGate(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	w := New(ctx)

	assert.True(t, w.containSupportType(cardmsg.InteractiveCard),
		"InteractiveCard(17) 必须在推送支持集,否则离线推送卡片分支不可达")
	// 对照:一个明确不支持的类型仍被门挡住(证明门本身有效,非恒真)。
	assert.False(t, w.containSupportType(common.ContentType(9999)),
		"未知类型应被 containSupportType 挡住")
}

func TestCardSenderTrusted(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	ensurePushAppBotTable(t, ctx)

	// iwh_ 合成身份 → 可信（无需查库）
	assert.True(t, cardSenderTrusted(ctx, "iwh_abc123"))

	// robot 表在册（status=1）→ 可信
	_, err := ctx.DB().InsertBySql("insert into robot(robot_id,status) values(?,1)", "bot_push_1").Exec()
	assert.NoError(t, err)
	assert.True(t, cardSenderTrusted(ctx, "bot_push_1"))

	// published app_bot 表在册（status=1）→ 可信；无需 robot 表兼容行。
	_, err = ctx.DB().InsertBySql(`
		INSERT INTO app_bot(id,uid,display_name,scope,status,token,created_by)
		VALUES('push_app','app_push_1','Push App','platform',1,'app_push_token_1','owner')`).Exec()
	assert.NoError(t, err)
	assert.True(t, cardSenderTrusted(ctx, "app_push_1"))

	// user.robot 只是展示元数据，不能单独建立卡片信任。
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO user(uid,name,robot,status) VALUES('presentation_only_push','Presentation Only',1,1)",
	).Exec()
	assert.NoError(t, err)
	assert.False(t, cardSenderTrusted(ctx, "presentation_only_push"))

	// 普通用户 → 不可信（直连长连接可伪造 type-17,plain 攻击者可控）
	assert.False(t, cardSenderTrusted(ctx, "human_9527"))
}

// 验收：推送 alert —— bot sender 取权威 plain(绝不出现原始卡 JSON);
// 非可信 sender 遮蔽为 [卡片]。
func TestGetMessageAlertCard(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	ensurePushAppBotTable(t, ctx)
	ctx.GetConfig().Push.ContentDetailOn = true

	_, err := ctx.DB().InsertBySql("insert into robot(robot_id,status) values(?,1)", "bot_push_2").Exec()
	assert.NoError(t, err)
	_, err = ctx.DB().InsertBySql(`
		INSERT INTO app_bot(id,uid,display_name,scope,status,token,created_by)
		VALUES('push_alert_app','app_push_alert','Push Alert App','platform',1,'app_push_alert_token','owner')`).Exec()
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

	alert, err = getMessageAlert(mk("app_push_alert"), toUser, ctx)
	assert.NoError(t, err)
	assert.Equal(t, "审批单 #42:待审批", alert, "published App Bot 推送正文 = 权威 plain")

	alert, err = getMessageAlert(mk("human_9527"), toUser, ctx)
	assert.NoError(t, err)
	assert.Equal(t, cardmsg.PlaceholderCard, alert, "非可信 sender 必须遮蔽为 [卡片]")
}
