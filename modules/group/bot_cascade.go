package group

import (
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/gocraft/dbr/v2"
)

// versionSeqer 抽象出 GenSeq 依赖以便单测直接驱动 cascadeRemoveBotsInvitedByUIDTx，
// 同时让 api / service 两路径直接传入各自已持有的 *config.Context。
type versionSeqer interface {
	GenSeq(key string) (int64, error)
}

// cascadeRemoveBotsInvitedByUIDTx 在事务内执行 D-2 bot 级联移除：
//   - 查询 inviterUID 在 groupNo 中拉入的活跃 bot（QueryBotsInvitedByUIDTx）
//   - 为每个 bot 生成新 member version + DeleteMemberTx
//   - 返回被级联删除的 bot uid 列表（外层按需用 userDB 查名字发系统 Tip）
//
// 任一 SQL 步失败直接返回 error，外层应 tx.Rollback()，保证「要么全成要么全不动」。
// 本函数不做 edge case（如 inviter 是否群主）判断，由调用方先判定再调。
func cascadeRemoveBotsInvitedByUIDTx(
	db *DB, seq versionSeqer, groupNo, inviterUID string, tx *dbr.Tx,
) ([]string, error) {
	if groupNo == "" || inviterUID == "" {
		return nil, nil
	}
	botUIDs, err := db.QueryBotsInvitedByUIDTx(groupNo, inviterUID, tx)
	if err != nil {
		return nil, fmt.Errorf("query bots invited by %s: %w", inviterUID, err)
	}
	if len(botUIDs) == 0 {
		return nil, nil
	}
	removed := make([]string, 0, len(botUIDs))
	for _, botUID := range botUIDs {
		v, err := seq.GenSeq(common.GroupMemberSeqKey)
		if err != nil {
			return removed, fmt.Errorf("gen bot cascade version: %w", err)
		}
		if err := db.DeleteMemberTx(groupNo, botUID, v, tx); err != nil {
			return removed, fmt.Errorf("delete bot member %s: %w", botUID, err)
		}
		removed = append(removed, botUID)
	}
	return removed, nil
}

// expandBlacklistTargetsWithOwnedBots 把拉黑/解除拉黑的目标 uid 列表扩展为
// 「用户本人 + 其名下在群 bot」，按原序去重后返回。
//
// #354 产品决策：bot 永远跟随其主人。拉黑用户时若不连带其 bot（旧行为：bot 仍
// status=Normal），被拉黑用户可经自己的 bot 旁路读群/子区内容，绕过
// ExistMemberActive 加固线（#343/#345）。解除拉黑走同一扩展，保证对称恢复。
//
// 查询失败返回 error，由调用方决定中断（拉黑是权限敏感操作，fail closed）。
func expandBlacklistTargetsWithOwnedBots(db *DB, groupNo string, uids []string) ([]string, error) {
	botUIDs, err := db.QueryBotUIDsOwnedByUIDs(groupNo, uids)
	if err != nil {
		return nil, fmt.Errorf("query bots owned by blacklist targets: %w", err)
	}
	if len(botUIDs) == 0 {
		return uids, nil
	}
	seen := make(map[string]struct{}, len(uids)+len(botUIDs))
	out := make([]string, 0, len(uids)+len(botUIDs))
	for _, uid := range uids {
		if _, ok := seen[uid]; ok {
			continue
		}
		seen[uid] = struct{}{}
		out = append(out, uid)
	}
	for _, uid := range botUIDs {
		if _, ok := seen[uid]; ok {
			continue
		}
		seen[uid] = struct{}{}
		out = append(out, uid)
	}
	return out, nil
}

// sendBotOwnerRemovedTip 发送「bot 所有者自助移除自己名下 bot」的系统消息
// （octo-web#1511）：
//
//	{owner} 将机器人 <bot1>、<bot2> 移出了群聊
//
// 为什么不复用 sendBotCascadeRemovedTip：那条的模板是「{leaver}{action}群聊，
// 其机器人 X 已一并移除」，描述的是「人走 bot 跟着走」；本场景所有者留在群里、
// 只撤走 bot，套用会把「所有者也离开了」这个错误事实写进群历史。
//
// 与级联 Tip 保持一致：type=common.Tip (2000)，NoPersist=0 保证新成员也能看到，
// bots 为空直接 no-op。名称插值口径同样对齐——沿用 UserBaseVo.Name，空则回落 UID。
func sendBotOwnerRemovedTip(ctx *config.Context, groupNo, ownerName string, bots []*config.UserBaseVo) error {
	if len(bots) == 0 || groupNo == "" {
		return nil
	}
	names := make([]string, 0, len(bots))
	for _, b := range bots {
		if b == nil {
			continue
		}
		name := b.Name
		if name == "" {
			name = b.UID
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil
	}
	if ownerName == "" {
		ownerName = "该用户"
	}
	content := fmt.Sprintf("%s 将机器人 %s 移出了群聊", ownerName, strings.Join(names, "、"))
	return ctx.SendMessage(&config.MsgSendReq{
		Header: config.MsgHeader{
			NoPersist: 0,
			RedDot:    0,
			SyncOnce:  0,
		},
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Payload: []byte(util.ToJson(map[string]interface{}{
			"content": content,
			"type":    common.Tip,
		})),
	})
}

// sendBotCascadeRemovedTip 发送 D-2 级联移除 bot 的系统消息。
//
// 场景：inviter 离群 / 被踢时，其邀请的 bot 被同事务级联移除。为避免「bot 神秘消失」
// 的用户体验，此处补一条透明的系统 Tip：
//
//	{leaver}<action>群聊，其机器人 <bot1>、<bot2> 已一并移除
//
// 参数：
//   - action 区分「离开了」/ 「被移出」
//   - botUsers 至少 1 个才发消息；空切片直接 no-op
//
// 故意不使用 SyncOnce / NoPersist 以保证新成员进群后也能看到历史提示。
// 消息 type=common.Tip (2000)，前端按普通系统提示渲染即可，无需新增类型。
func sendBotCascadeRemovedTip(ctx *config.Context, groupNo, leaverName, action string, botUsers []*user.Model) error {
	if len(botUsers) == 0 || groupNo == "" {
		return nil
	}
	names := make([]string, 0, len(botUsers))
	for _, b := range botUsers {
		if b == nil {
			continue
		}
		name := b.Name
		if name == "" {
			name = b.UID
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil
	}
	if leaverName == "" {
		leaverName = "该用户"
	}
	if action == "" {
		action = "离开了"
	}
	content := fmt.Sprintf("%s%s群聊，其机器人 %s 已一并移除", leaverName, action, strings.Join(names, "、"))
	return ctx.SendMessage(&config.MsgSendReq{
		Header: config.MsgHeader{
			NoPersist: 0,
			RedDot:    0,
			SyncOnce:  0,
		},
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Payload: []byte(util.ToJson(map[string]interface{}{
			"content": content,
			"type":    common.Tip,
		})),
	})
}
