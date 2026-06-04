package group

import (
	"errors"
	"fmt"
	"math"
	"os"
	"runtime/debug"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkevent"
	"github.com/Mininglamp-OSS/octo-server/modules/base/event"
	"go.uber.org/zap"
)

// Action-layer sentinel errors. groupSettingUpdate's dispatch classifies these
// via errors.Is so client-facing failures are not collapsed into Internal=true
// 500 (ErrGroupStoreFailed). Genuine DB/event failures keep returning their own
// errors and fall through to the store_failed fallback.
var (
	// errSettingInvalidValueType marks a malformed / wrong-typed setting value (client 400).
	errSettingInvalidValueType = errors.New("invalid value type")
	// errSettingAllowExternalRange marks an out-of-range allow_external value (client 400).
	errSettingAllowExternalRange = errors.New("allow_external only accepts 0 or 1")
	// errSettingAllowNoMentionRange marks an out-of-range allow_no_mention value (client 400).
	errSettingAllowNoMentionRange = errors.New("allow_no_mention only accepts 0 or 1")
	// errGroupUpdateForbidden marks a non-manager/creator attempting a group-attr update (client 403).
	errGroupUpdateForbidden = errors.New("没有权限！")
)

// safeIntFromFloat64 safely converts an interface{} to int via float64.
func safeIntFromFloat64(v interface{}) (int, bool) {
	f, ok := v.(float64)
	if !ok {
		return 0, false
	}
	return int(f), true
}

// strictIntFromFloat64 converts an interface{} to int via float64 but rejects
// any JSON number carrying a fractional part. Plain safeIntFromFloat64
// truncates first (0.9→0, 1.9→1), so a fractional value could sneak past a
// later 0/1 range check and flip a group switch. Used for the allow_no_mention
// path (YUJ-2996 Blocking 2): a non-integer JSON value is rejected up front so
// only true integers reach the 0/1 range check.
func strictIntFromFloat64(v interface{}) (int, bool) {
	f, ok := v.(float64)
	if !ok {
		return 0, false
	}
	if f != math.Trunc(f) {
		return 0, false
	}
	return int(f), true
}

// safeString safely converts an interface{} to string.
func safeString(v interface{}) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

type settingContext struct {
	loginUID     string
	loginName    string
	groupSetting *Setting
	newSetting   bool
	g            *Group
}

func (c *settingContext) updateGroupSetting() error {
	if c.newSetting {
		err := c.g.settingDB.InsertSetting(c.groupSetting)
		if err != nil {
			return err
		}
	} else {
		err := c.g.settingDB.UpdateSetting(c.groupSetting)
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *settingContext) sendChannelUpdate() error {
	return c.g.ctx.SendChannelUpdate(config.ChannelReq{
		ChannelID:   c.loginUID,
		ChannelType: common.ChannelTypePerson.Uint8(),
	}, config.ChannelReq{
		ChannelID:   c.groupSetting.GroupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
	})
}

func (c *settingContext) updateSettingAndSendCMD() error {
	err := c.updateGroupSetting()
	if err != nil {
		return err
	}
	return c.sendChannelUpdate()
}

type groupUpdateContext struct {
	loginUID   string
	loginName  string
	groupModel *Model
	g          *Group
}

func (g *groupUpdateContext) isManager() (bool, error) {
	isManager, err := g.g.db.QueryIsGroupManagerOrCreator(g.groupModel.GroupNo, g.loginUID)
	if err != nil {
		g.g.Error("查询是否是群管理者失败！", zap.Error(err))
		return false, err
	}
	return isManager, nil
}

func (g *groupUpdateContext) checkPermissions() error {
	isManager, err := g.isManager()
	if err != nil {
		return err
	}
	if !isManager {
		return errGroupUpdateForbidden
	}
	return nil
}

func (g *groupUpdateContext) updateGroup() error {
	return g.g.db.Update(g.groupModel)
}

func (g *groupUpdateContext) commmitGroupUpdateEvent(key, value string) error {
	tx, err := g.g.ctx.DB().Begin()
	if err != nil {
		g.g.Error("开启事务失败！", zap.Error(err))
		return errors.New("开启事务失败！")
	}
	defer func() {
		if err := recover(); err != nil {
			tx.RollbackUnlessCommitted()
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
		}
	}()
	groupNo := g.groupModel.GroupNo
	// 发布群信息更新事件
	eventID, err := g.g.ctx.EventBegin(&wkevent.Data{
		Event: event.GroupUpdate,
		Type:  wkevent.Message,
		Data: &config.MsgGroupUpdateReq{
			GroupNo:      groupNo,
			Operator:     g.loginUID,
			OperatorName: g.loginName,
			Attr:         key,
			Data: map[string]string{
				key: value,
			},
		},
	}, tx)
	if err != nil {
		tx.Rollback()
		g.g.Error("开启群更新事件失败！", zap.Error(err))
		return errors.New("开启群更新事件失败！")
	}
	if err := tx.Commit(); err != nil {
		tx.RollbackUnlessCommitted()
		g.g.Error("提交事务失败！", zap.Error(err))
		return errors.New("提交事务失败！")
	}
	g.g.ctx.EventCommit(eventID)

	g.g.ctx.SendChannelUpdateToGroup(groupNo) // 发送频道更新cmd

	return nil
}

type groupUpdateActionFnc func(ctx *groupUpdateContext, value interface{}) error

type groupSettingActionFnc func(ctx *settingContext, value interface{}) error

// 设置action
var settingActionMap = map[string]groupSettingActionFnc{
	"mute": func(ctx *settingContext, value interface{}) error { // 免打扰
		val, ok := safeIntFromFloat64(value)
		if !ok {
			return errSettingInvalidValueType
		}
		ctx.groupSetting.Mute = val
		return ctx.updateSettingAndSendCMD()
	},
	"top": func(ctx *settingContext, value interface{}) error { // 会话置顶
		val, ok := safeIntFromFloat64(value)
		if !ok {
			return errSettingInvalidValueType
		}
		ctx.groupSetting.Top = val
		return ctx.updateSettingAndSendCMD()
	},
	"save": func(ctx *settingContext, value interface{}) error { // 保存群
		val, ok := safeIntFromFloat64(value)
		if !ok {
			return errSettingInvalidValueType
		}
		ctx.groupSetting.Save = val
		return ctx.updateSettingAndSendCMD()
	},
	"show_nick": func(ctx *settingContext, value interface{}) error { // 是否显示昵称
		val, ok := safeIntFromFloat64(value)
		if !ok {
			return errSettingInvalidValueType
		}
		ctx.groupSetting.ShowNick = val
		return ctx.updateSettingAndSendCMD()
	},
	"chat_pwd_on": func(ctx *settingContext, value interface{}) error { // 聊天密码
		val, ok := safeIntFromFloat64(value)
		if !ok {
			return errSettingInvalidValueType
		}
		ctx.groupSetting.ChatPwdOn = val
		return ctx.updateSettingAndSendCMD()
	},
	"screenshot": func(ctx *settingContext, value interface{}) error { // 截屏
		val, ok := safeIntFromFloat64(value)
		if !ok {
			return errSettingInvalidValueType
		}
		ctx.groupSetting.Screenshot = val
		return ctx.updateSettingAndSendCMD()
	},
	"join_group_remind": func(ctx *settingContext, value interface{}) error { // 进群提醒
		val, ok := safeIntFromFloat64(value)
		if !ok {
			return errSettingInvalidValueType
		}
		ctx.groupSetting.JoinGroupRemind = val
		return ctx.updateSettingAndSendCMD()
	},
	"revoke_remind": func(ctx *settingContext, value interface{}) error { // 撤回提醒
		val, ok := safeIntFromFloat64(value)
		if !ok {
			return errSettingInvalidValueType
		}
		ctx.groupSetting.RevokeRemind = val
		return ctx.updateSettingAndSendCMD()
	},
	"receipt": func(ctx *settingContext, value interface{}) error { // 消息已读回执
		val, ok := safeIntFromFloat64(value)
		if !ok {
			return errSettingInvalidValueType
		}
		ctx.groupSetting.Receipt = val
		return ctx.updateSettingAndSendCMD()
	},
	"remark": func(ctx *settingContext, value interface{}) error { // 群备注
		val, ok := safeString(value)
		if !ok {
			return errSettingInvalidValueType
		}
		ctx.groupSetting.Remark = val
		return ctx.updateSettingAndSendCMD()
	},
	"flame": func(ctx *settingContext, value interface{}) error { // 阅后即焚开启
		val, ok := safeIntFromFloat64(value)
		if !ok {
			return errSettingInvalidValueType
		}
		ctx.groupSetting.Flame = val
		return ctx.updateSettingAndSendCMD()
	},
	"flame_second": func(ctx *settingContext, value interface{}) error { // 阅后即焚时间
		val, ok := safeIntFromFloat64(value)
		if !ok {
			return errSettingInvalidValueType
		}
		ctx.groupSetting.FlameSecond = val
		return ctx.updateSettingAndSendCMD()
	},
}

var groupUpdateActionMap = map[string]groupUpdateActionFnc{
	common.GroupAttrKeyForbidden: func(ctx *groupUpdateContext, value interface{}) error { // 群内禁言
		if err := ctx.checkPermissions(); err != nil {
			return err
		}
		val, ok := safeIntFromFloat64(value)
		if !ok {
			return errSettingInvalidValueType
		}
		ctx.groupModel.Forbidden = val

		err := ctx.updateGroup()
		if err != nil {
			return err
		}

		groupNo := ctx.groupModel.GroupNo

		whitelistUIDs := make([]string, 0)
		if ctx.groupModel.Forbidden == 1 {
			managerOrCreaterUIDs, err := ctx.g.db.QueryGroupManagerOrCreatorUIDS(groupNo)
			if err != nil {
				return err
			}
			whitelistUIDs = managerOrCreaterUIDs
		}
		err = ctx.g.ctx.IMWhitelistSet(config.ChannelWhitelistReq{
			ChannelReq: config.ChannelReq{
				ChannelID:   groupNo,
				ChannelType: common.ChannelTypeGroup.Uint8(),
			},
			UIDs: whitelistUIDs,
		})
		if err != nil {
			ctx.g.Error("设置禁言失败！", zap.Error(err))
			return errors.New("设置禁言失败！")
		}

		ctx.commmitGroupUpdateEvent(common.GroupAttrKeyForbidden, fmt.Sprintf("%d", ctx.groupModel.Forbidden))

		return nil
	},
	common.GroupAttrKeyForbiddenAddFriend: func(ctx *groupUpdateContext, value interface{}) error { // 群内禁止加好友
		if err := ctx.checkPermissions(); err != nil {
			return err
		}
		val, ok := safeIntFromFloat64(value)
		if !ok {
			return errSettingInvalidValueType
		}
		ctx.groupModel.ForbiddenAddFriend = val
		err := ctx.updateGroup()
		if err != nil {
			return err
		}
		groupNo := ctx.groupModel.GroupNo
		// 通知群内成员更新频道
		err = ctx.g.ctx.SendChannelUpdateToGroup(groupNo)

		return err
	},
	common.GroupAttrKeyInvite: func(ctx *groupUpdateContext, value interface{}) error { // 邀请开关
		if err := ctx.checkPermissions(); err != nil {
			return err
		}
		val, ok := safeIntFromFloat64(value)
		if !ok {
			return errSettingInvalidValueType
		}
		ctx.groupModel.Invite = val

		err := ctx.updateGroup()
		if err != nil {
			return err
		}

		return ctx.commmitGroupUpdateEvent(common.GroupAttrKeyInvite, fmt.Sprintf("%d", ctx.groupModel.Invite))
	},
	common.GroupAllowViewHistoryMsg: func(ctx *groupUpdateContext, value interface{}) error {
		if err := ctx.checkPermissions(); err != nil {
			return err
		}
		val, ok := safeIntFromFloat64(value)
		if !ok {
			return errSettingInvalidValueType
		}
		ctx.groupModel.AllowViewHistoryMsg = val

		err := ctx.updateGroup()
		if err != nil {
			return err
		}
		groupNo := ctx.groupModel.GroupNo
		// 通知群内成员更新频道
		return ctx.g.ctx.SendChannelUpdateToGroup(groupNo)
	},
	common.GroupAllowMemberPinnedMessage: func(ctx *groupUpdateContext, value interface{}) error {
		if err := ctx.checkPermissions(); err != nil {
			return err
		}
		val, ok := safeIntFromFloat64(value)
		if !ok {
			return errSettingInvalidValueType
		}
		ctx.groupModel.AllowMemberPinnedMessage = val
		err := ctx.updateGroup()
		if err != nil {
			return err
		}
		groupNo := ctx.groupModel.GroupNo
		// 通知群内成员更新频道
		return ctx.g.ctx.SendChannelUpdateToGroup(groupNo)
	},
	GroupAttrKeyAllowExternal: func(ctx *groupUpdateContext, value interface{}) error { // 是否允许外部成员加入
		if err := ctx.checkPermissions(); err != nil {
			return err
		}
		val, ok := safeIntFromFloat64(value)
		if !ok {
			return errSettingInvalidValueType
		}
		if val != 0 && val != 1 {
			return errSettingAllowExternalRange
		}
		ctx.groupModel.AllowExternal = val
		if err := ctx.updateGroup(); err != nil {
			return err
		}
		return ctx.commmitGroupUpdateEvent(GroupAttrKeyAllowExternal, fmt.Sprintf("%d", ctx.groupModel.AllowExternal))
	},
	GroupAttrKeyAllowNoMention: func(ctx *groupUpdateContext, value interface{}) error { // 群级是否允许免@生效
		if err := ctx.checkPermissions(); err != nil {
			return err
		}
		// strictIntFromFloat64 (not safeIntFromFloat64): reject fractional JSON
		// values up front so e.g. 0.9 / 1.9 can't truncate into a valid 0/1 and
		// silently flip the group switch (YUJ-2996 Blocking 2).
		val, ok := strictIntFromFloat64(value)
		if !ok {
			return errSettingInvalidValueType
		}
		if val != 0 && val != 1 {
			return errSettingAllowNoMentionRange
		}
		ctx.groupModel.AllowNoMention = val
		if err := ctx.updateGroup(); err != nil {
			return err
		}
		// 静默群属性开关：只发不可见的频道刷新 cmd（同 mute / AllowViewHistoryMsg /
		// AllowMemberPinnedMessage），不走 commmitGroupUpdateEvent。后者会发布
		// GroupUpdate + wkevent.Message，客户端当成系统消息渲染并产生未读红点，而
		// 客户端没有 allow_no_mention 的本地化模板，结果渲染成「只有用户名、无内容」
		// 的空白公告（YUJ-3153 Bug 3）。allow_external 仍走 commmitGroupUpdateEvent，
		// 本单不动以避免回归。
		return ctx.g.ctx.SendChannelUpdateToGroup(ctx.groupModel.GroupNo)
	},
}

// GroupAttrKeyAllowExternal 是否允许外部成员加入群的群属性 key。
// 定义在本模块而非 dmwork-lib.common，因为这是 OCTO 扩展属性，未进入上游 lib。
const GroupAttrKeyAllowExternal = "allow_external"

// GroupAttrKeyAllowNoMention 群级「允许免@生效」总开关的群属性 key。群主/管理员可控，
// 与 bot 主人的 bot_mention_pref 两轴 AND：最终免@ = bot主人开了本群免@ AND 群管理员允许本群免@。
// 同为 OCTO 扩展属性，未进入上游 lib。
const GroupAttrKeyAllowNoMention = "allow_no_mention"
