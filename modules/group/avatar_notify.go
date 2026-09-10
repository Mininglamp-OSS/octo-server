package group

import (
	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
)

// sendGroupAvatarChangedMessage 在群头像实际变更后发一条与改名同款的系统消息。
//
// 文案走 GroupUpdate(1005) + "{0}修改了群头像" + extra 操作者，客户端 SystemCell
// 会把 {0} 替换成 extra[0].name，呈现为「XXX修改了群头像」。不改 octo-lib 的
// SendGroupUpdate：那边没有 avatar 分支，未知 attr 只会发出 "{0}"（只有用户名、
// 没有动作文案）。CMD groupAvatarUpdate / channelUpdate 仍负责即时刷头像，本消息
// 只进会话历史。best-effort：调用方在落库成功后发送，失败只打日志。
func sendGroupAvatarChangedMessage(ctx *config.Context, groupNo, operatorUID, operatorName string) error {
	if ctx == nil || groupNo == "" {
		return nil
	}
	return ctx.SendMessage(&config.MsgSendReq{
		Header: config.MsgHeader{
			NoPersist: 0,
			RedDot:    1,
			SyncOnce:  0,
		},
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Payload: []byte(util.ToJson(map[string]interface{}{
			"content": "{0}修改了群头像",
			"extra": []config.UserBaseVo{
				{
					UID:  operatorUID,
					Name: operatorName,
				},
			},
			"type": common.GroupUpdate,
		})),
	})
}
