package bot_api

// card-message-interaction P2 D10.6：bot 清除自己卡片的修订历史（可审计的擦除）。
// 删除该消息的全部非墓碑帧并写一条墓碑行（editor+时间+清除帧数），墓碑本身出现在
// 历史列表里 —— 擦除有据可查，不是静默删除。
//
// POST /v1/bot/message/card/revisions/clear —— bot-token 鉴权（与其它 bot_api 路由
// 同中间件链），属主校验（消息 from_uid == 调用 bot）沿用 botMessageEdit 的
// YUJ-60-lineage 归属守卫。

import (
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"go.uber.org/zap"
)

// botCardRevisionsClear handles POST /v1/bot/message/card/revisions/clear.
func (ba *BotAPI) botCardRevisionsClear(c *wkhttp.Context) {
	var req struct {
		MessageID   string `json:"message_id"`
		ChannelID   string `json:"channel_id"`
		ChannelType uint8  `json:"channel_type"`
	}
	if err := c.BindJSON(&req); err != nil {
		respondBotAPIRequestInvalid(c, "")
		return
	}
	if strings.TrimSpace(req.MessageID) == "" {
		respondBotAPIRequestInvalid(c, "message_id")
		return
	}
	if strings.TrimSpace(req.ChannelID) == "" {
		respondBotAPIRequestInvalid(c, "channel_id")
		return
	}
	if req.ChannelType == 0 {
		respondBotAPIRequestInvalid(c, "channel_type")
		return
	}
	if !cardmsg.BotEnabled() {
		// bot 侧有效门禁（总开关 AND bot 子开关 OCTO_BOT_CARD_ENABLED），与发卡
		// 路径及 /v1/bot/card/profile.enabled 同源。
		httperr.ResponseErrorL(c, errcode.ErrBotAPICardDisabled, nil, nil)
		return
	}
	robotID := getRobotIDFromContext(c)

	msgIDInt, perr := strconv.ParseInt(req.MessageID, 10, 64)
	if perr != nil {
		respondBotAPIRequestInvalid(c, "message_id")
		return
	}
	// 属主 + 卡片校验：按 message_id 查存储消息，验证 sender==调用 bot（YUJ-60 归属）
	// 且为 type-17 卡片（对非卡片写墓碑无意义）。
	syncResp, err := ba.ctx.IMSearchMessages(&config.MsgSearchReq{
		ChannelID:   req.ChannelID,
		ChannelType: req.ChannelType,
		MessageIds:  []int64{msgIDInt},
		LoginUID:    robotID,
	})
	if err != nil {
		ba.Error("查询卡片修订清除目标消息失败", zap.Error(err), zap.String("messageID", req.MessageID))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
		return
	}
	if syncResp == nil || len(syncResp.Messages) == 0 {
		httperr.ResponseErrorL(c, errcode.ErrBotAPIMessageNotFound, nil, nil)
		return
	}
	if syncResp.Messages[0].FromUID != robotID {
		ba.Warn("非属主 bot 清除卡片修订,拒绝", zap.String("fromUID", syncResp.Messages[0].FromUID), zap.String("robotID", robotID))
		httperr.ResponseErrorL(c, errcode.ErrBotAPICardRevisionClearForbidden, nil, nil)
		return
	}
	if !cardmsg.IsCardRawPayload(syncResp.Messages[0].Payload) {
		respondBotAPIRequestInvalid(c, "message_id")
		return
	}

	// 墓碑行的 channel_id 与帧行同口径（person 频道用 fakeChannelID）。
	fakeChannelID := req.ChannelID
	if req.ChannelType == common.ChannelTypePerson.Uint8() {
		fakeChannelID = common.GetFakeChannelIDWith(robotID, req.ChannelID)
	}
	cleared, err := ba.cardRevisions.Clear(req.MessageID, fakeChannelID, req.ChannelType, robotID, time.Now().Unix())
	if err != nil {
		ba.Error("清除卡片修订失败", zap.Error(err), zap.String("messageID", req.MessageID))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIStoreFailed, nil, nil)
		return
	}
	c.Response(map[string]interface{}{"cleared": cleared})
}
