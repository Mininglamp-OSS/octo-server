package user

import (
	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"go.uber.org/zap"
)

// Setting 用户设置
type Setting struct {
	ctx *config.Context
	log.Log
	db *SettingDB
}

// NewSetting 创建
func NewSetting(ctx *config.Context) *Setting {
	return &Setting{ctx: ctx, Log: log.NewTLog("UserSetting"), db: NewSettingDB(ctx.DB())}
}

// 用户设置
func (u *Setting) userSettingUpdate(c *wkhttp.Context) {
	loginUID := c.MustGet("uid").(string)
	toUID := c.Param("uid")
	var settingMap map[string]interface{}
	if err := c.BindJSON(&settingMap); err != nil {
		u.Error("数据格式有误！", zap.Error(err))
		respondUserRequestInvalid(c, "")
		return
	}
	model, err := u.db.QueryUserSettingModel(toUID, loginUID)
	if err != nil {
		u.Error("查询用户设置失败！", zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	insert := false // 是否是插入操作
	if model == nil {
		insert = true // 是否是插入操作
		model = newDefaultSettingModel()
		model.UID = loginUID
		model.ToUID = toUID
	}
	for key, value := range settingMap {
		switch key {
		case "mute":
			if f, ok := value.(float64); ok {
				model.Mute = int(f)
			}
		case "top":
			if f, ok := value.(float64); ok {
				model.Top = int(f)
			}
		case "chat_pwd_on":
			if f, ok := value.(float64); ok {
				model.ChatPwdOn = int(f)
			}
		case "screenshot":
			if f, ok := value.(float64); ok {
				model.Screenshot = int(f)
			}
		case "revoke_remind":
			if f, ok := value.(float64); ok {
				model.RevokeRemind = int(f)
			}
		case "receipt":
			if f, ok := value.(float64); ok {
				model.Receipt = int(f)
			}
		case "flame":
			if f, ok := value.(float64); ok {
				model.Flame = int(f)
			}
		case "flame_second":
			if f, ok := value.(float64); ok {
				model.FlameSecond = int(f)
			}
		case "remark":
			if s, ok := value.(string); ok {
				model.Remark = s
			}
		}
	}
	version, err := u.ctx.GenSeq(common.UserSettingSeqKey)
	if err != nil {
		u.Error("生成用户设置序列号失败", zap.Error(err))
		respondUserError(c, errcode.ErrUserStoreFailed)
		return
	}
	model.Version = version
	if insert {
		err = u.db.InsertUserSettingModel(model)
		if err != nil {
			u.Error("添加设置失败！", zap.Error(err))
			respondUserError(c, errcode.ErrUserStoreFailed)
			return
		}
	} else {
		err = u.db.UpdateUserSettingModel(model)
		if err != nil {
			u.Error("修改设置失败！", zap.Error(err))
			respondUserError(c, errcode.ErrUserStoreFailed)
			return
		}
	}
	// 发送一个频道更新命令 发给自己的其他设备，如果其他设备在线的话
	err = u.ctx.SendCMD(config.MsgCMDReq{
		ChannelID:   loginUID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		CMD:         common.CMDChannelUpdate,
		Param: map[string]interface{}{
			"channel_id":   toUID,
			"channel_type": common.ChannelTypePerson,
		},
	})
	if err != nil {
		u.Error("发送频道更新命令失败！", zap.Error(err))
		respondUserError(c, errcode.ErrUserIMCallFailed)
		return
	}
	c.ResponseOK()
}
