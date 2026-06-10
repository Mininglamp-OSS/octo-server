package user

import (
	"fmt"
	"net/http"
	"os"
	"runtime/debug"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkevent"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/base/event"
	chservice "github.com/Mininglamp-OSS/octo-server/modules/channel/service"
	"github.com/Mininglamp-OSS/octo-server/modules/conversation_ext"
	"github.com/Mininglamp-OSS/octo-server/modules/source"
	"github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	wkutil "github.com/Mininglamp-OSS/octo-server/pkg/util"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

// Friend 好友
type Friend struct {
	ctx *config.Context
	log.Log
	db            *friendDB
	settingDB     *SettingDB
	userDB        *DB
	onlineService IOnlineService
	userService   IService
}

// NewFriend 创建
func NewFriend(ctx *config.Context) *Friend {
	f := &Friend{
		ctx:           ctx,
		Log:           log.NewTLog("Friend"),
		userDB:        NewDB(ctx),
		db:            newFriendDB(ctx),
		onlineService: NewOnlineService(ctx),
		settingDB:     NewSettingDB(ctx.DB()),
		userService:   NewService(ctx),
	}
	f.ctx.AddEventListener(event.FriendSure, f.handleFriendSure)
	f.ctx.AddEventListener(event.FriendDelete, f.handleDeleteFriend)
	f.ctx.AddEventListener(event.EventUserRegister, f.handleUserRegister)
	return f
}

// Route 配置路由规则
func (f *Friend) Route(r *wkhttp.WKHttp) {
	uidLimit := appwkhttp.SharedUIDRateLimiter(r, f.ctx)
	friend := r.Group("/v1/friend", f.ctx.AuthMiddleware(r), uidLimit)
	{
		friend.POST("/apply", f.friendApply)           // 好友申请
		friend.GET("/apply", f.apply)                  // 好友申请列表
		friend.DELETE("/apply/:to_uid", f.deleteApply) // 删除好友申请
		friend.PUT("/refuse/:to_uid", f.refuseApply)   // 拒绝申请
		friend.POST("/sure", f.friendSure)             // 好友确认
		friend.GET("/sync", f.friendSync)              // 同步好友
		friend.GET("/search", f.friendSearch)          // 查询好友
		friend.PUT("/remark", f.remark)                //好友备注
	}
	friends := r.Group("/v1/friends", f.ctx.AuthMiddleware(r), uidLimit)
	{
		friends.DELETE("/:uid", f.delete) //删除好友
	}
}

// 拒绝申请
func (f *Friend) refuseApply(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	toUid := c.Param("to_uid")
	if toUid == "" {
		respondUserRequestInvalid(c, "uid")
		return
	}

	apply, err := f.db.queryApplyWithUidAndToUid(loginUID, toUid)
	if err != nil {
		f.Error("查询申请记录错误", zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	if apply == nil || apply.UID != loginUID {
		respondUserError(c, errcode.ErrUserFriendApplyNotFound)
		return
	}
	apply.Status = 2
	err = f.db.updateApply(apply)
	if err != nil {
		f.Error("修改申请记录错误", zap.Error(err))
		respondUserError(c, errcode.ErrUserStoreFailed)
		return
	}
	c.ResponseOK()
}

// 删除好友申请记录
func (f *Friend) deleteApply(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	toUid := c.Param("to_uid")
	if toUid == "" {
		respondUserRequestInvalid(c, "id")
		return
	}
	err := f.db.deleteApplyWithUidAndToUid(loginUID, toUid)
	if err != nil {
		f.Error("删除申请记录错误", zap.Error(err))
		respondUserError(c, errcode.ErrUserStoreFailed)
		return
	}
	c.ResponseOK()
}

// 好友申请列表
func (f *Friend) apply(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	page := c.Query("page_index")
	size := c.Query("page_size")
	pageIndex := wkutil.AtoiOrDefault(page, 1)
	pageSize := wkutil.AtoiOrDefault(size, 20)
	applys, err := f.db.queryApplysWithPage(loginUID, uint64(pageSize), uint64(pageIndex))
	if err != nil {
		f.Error("查询好友申请列表错误", zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	list := make([]*friendApplyResp, 0)
	if len(applys) > 0 {
		uids := make([]string, 0)
		for _, apply := range applys {
			uids = append(uids, apply.ToUID)
		}
		users, err := f.userService.GetUsers(uids)
		if err != nil {
			f.Error("查询申请用户信息错误", zap.Error(err))
			respondUserError(c, errcode.ErrUserQueryFailed)
			return
		}
		if len(users) == 0 {
			respondUserError(c, errcode.ErrUserNotFound)
			return
		}
		userMap := make(map[string]string, len(users))
		for _, user := range users {
			userMap[user.UID] = user.Name
		}
		for _, apply := range applys {
			list = append(list, &friendApplyResp{
				Id:        apply.Id,
				UID:       apply.UID,
				ToUID:     apply.ToUID,
				ToName:    userMap[apply.ToUID],
				Remark:    apply.Remark,
				Status:    apply.Status,
				Token:     apply.Token,
				CreatedAt: apply.CreatedAt.String(),
			})
		}
	}
	c.Response(list)
}

// 删除好友
func (f *Friend) delete(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	uid := c.Param("uid")
	if uid == "" {
		respondUserRequestInvalid(c, "uid")
		return
	}
	tx, err := f.ctx.DB().Begin()
	if err != nil {
		f.Error("开启事务失败！", zap.Error(err))
		respondUserError(c, errcode.ErrUserStoreFailed)
		return
	}
	defer func() {
		if err := recover(); err != nil {
			tx.RollbackUnlessCommitted()
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
		}
	}()
	version, err := f.ctx.GenSeq(common.FriendSeqKey)
	if err != nil {
		tx.Rollback()
		f.Error("生成好友关系序列号失败", zap.Error(err))
		respondUserError(c, errcode.ErrUserStoreFailed)
		return
	}
	// err = f.db.updateRelationshipTx(loginUID, uid, 1, 1, "", version, tx) // 不能删除sourceVercode 如果删除了 已有会话发起加好友会提示验证码不为空
	err = f.db.updateRelationship2Tx(loginUID, uid, 1, 1, version, tx)
	if err != nil {
		util.CheckErr(tx.Rollback())
		f.Error("删除好友错误", zap.Error(err))
		respondUserError(c, errcode.ErrUserStoreFailed)
		return
	}
	err = f.db.updateAloneTx(uid, loginUID, 1, tx)
	if err != nil {
		util.CheckErr(tx.Rollback())
		f.Error("修改好友单项关系错误", zap.Error(err))
		respondUserError(c, errcode.ErrUserStoreFailed)
		return
	}
	// 发布删除好友事件
	eventID, err := f.ctx.EventBegin(&wkevent.Data{
		Event: event.FriendDelete,
		Type:  wkevent.Message,
		Data: map[string]interface{}{
			"uid":    loginUID,
			"to_uid": uid,
		},
	}, tx)
	if err != nil {
		f.Error("发送删除好友事件失败", zap.Error(err))
		tx.Rollback()
		respondUserError(c, errcode.ErrUserStoreFailed)
		return
	}
	userSetting, err := f.settingDB.querySettingByUIDAndToUID(loginUID, uid)
	if err != nil {
		tx.Rollback()
		f.Error("查询用户好友设置错误", zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	if userSetting != nil {
		userSetting.ChatPwdOn = 0
		userSetting.Top = 0
		userSetting.Mute = 0
		userSetting.Receipt = 1
		userSetting.Screenshot = 1
		userSetting.RevokeRemind = 0
		userSetting.Remark = ""
		userSetting.Flame = 0
		userSetting.FlameSecond = 0
		err := f.settingDB.updateUserSettingModelWithToUIDTx(userSetting, loginUID, uid, tx)
		if err != nil {
			tx.Rollback()
			f.Error("重置好友设置错误", zap.Error(err))
			respondUserError(c, errcode.ErrUserStoreFailed)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		tx.RollbackUnlessCommitted()
		f.Error("提交事务失败！", zap.Error(err))
		respondUserError(c, errcode.ErrUserStoreFailed)
		return
	}
	f.ctx.EventCommit(eventID)

	err = f.ctx.SendChannelUpdate(config.ChannelReq{
		ChannelID:   uid,
		ChannelType: common.ChannelTypePerson.Uint8(),
	}, config.ChannelReq{
		ChannelID:   loginUID,
		ChannelType: common.ChannelTypePerson.Uint8(),
	})
	if err != nil {
		f.Warn("发送频道更新命令失败！", zap.Error(err))
	}

	err = f.ctx.SendFriendDelete(&config.MsgFriendDeleteReq{
		FromUID: loginUID,
		ToUID:   uid,
	})
	if err != nil {
		f.Error("发送删除好友的cmd失败！", zap.Error(err))
	}

	// 清理双方的好友置顶
	RemovePinnedForUser(loginUID, uid, common.ChannelTypePerson.Uint8())
	RemovePinnedForUser(uid, loginUID, common.ChannelTypePerson.Uint8())
	// 级联清理双方的 DM ext 关注行
	conversation_ext.RemoveConvExtForUser(loginUID, uid)
	conversation_ext.RemoveConvExtForUser(uid, loginUID)

	c.ResponseOK()
}

// 好友申请
func (f *Friend) friendApply(c *wkhttp.Context) {
	fromUID := c.GetLoginUID()
	fromName := c.GetLoginName()

	var req applyReq
	if err := c.BindJSON(&req); err != nil {
		f.Error(common.ErrData.Error(), zap.Error(err))
		respondUserRequestInvalid(c, "")
		return
	}
	if err := req.Check(); err != nil {
		// applyReq/sureReq.Check 返回"X 不能为空"类输入校验错误，统一走
		// request_invalid（不做 per-field 标注，与 /v1/user login 一致）。
		respondUserRequestInvalid(c, "")
		return
	}
	if fromUID == req.ToUID {
		respondUserError(c, errcode.ErrUserCannotAddSelf)
		return
	}
	loginUserInfo, err := f.userDB.QueryByUID(fromUID)
	if err != nil {
		f.Error("查询用户信息错误", zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	if loginUserInfo == nil || loginUserInfo.IsDestroy == IsDestroyDone || loginUserInfo.Status != 1 {
		f.Error("登录用户不存在！", zap.String("uid", fromUID))
		respondUserError(c, errcode.ErrUserCurrentNotFound)
		return
	}
	// 是否是好友
	isFriendLoginUser, err := f.db.IsFriend(fromUID, req.ToUID)
	if err != nil {
		f.Error("查询是否是好友失败！", zap.Error(err), zap.String("uid", fromUID), zap.String("toUid", req.ToUID))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	isFriendToUser, err := f.db.IsFriend(req.ToUID, fromUID)
	if err != nil {
		f.Error("查询是否是好友失败！", zap.Error(err), zap.String("uid", fromUID), zap.String("toUid", req.ToUID))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	if isFriendLoginUser && isFriendToUser {
		respondUserError(c, errcode.ErrUserAlreadyFriend)
		return
	}

	toUser, err := f.userDB.QueryByUID(req.ToUID)
	if err != nil {
		f.Error("查询接收者用户信息失败！", zap.Error(err), zap.String("uid", fromUID))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	if toUser == nil || toUser.IsDestroy == IsDestroyDone {
		f.Error("接收好友请求的用户不存在！", zap.String("to_uid", req.ToUID))
		respondUserError(c, errcode.ErrUserNotFound)
		return
	}
	// Bot 用户（robot=1）：检查 Space 隔离，跳过 vercode 验证
	isBotTarget := toUser.Robot == 1
	systemBots := map[string]bool{"botfather": true, "u_10000": true}
	if isBotTarget && !systemBots[req.ToUID] {
		commonSpace := space.GetCommonSpaceID(f.ctx, fromUID, req.ToUID)
		if commonSpace == "" {
			respondUserError(c, errcode.ErrUserBotNotInSpace)
			return
		}
	}
	verifyVercode := true
	if req.Vercode == "" {
		if isBotTarget {
			// Bot 不需要 vercode
			verifyVercode = false
			req.Vercode = toUser.Vercode // 用 Bot 自身的 vercode
		} else {
			friend, err := f.db.queryWithUID(fromUID, req.ToUID)
			if err != nil {
				f.Error("查询好友信息错误", zap.Error(err), zap.String("to_uid", req.ToUID))
				respondUserError(c, errcode.ErrUserQueryFailed)
				return
			}
			if friend == nil {
				f.Error("好友信息不存在", zap.String("to_uid", req.ToUID))
				respondUserError(c, errcode.ErrUserFriendNotFound)
				return
			}
			if friend.SourceVercode == "" {
				f.Error("验证码不能为空", zap.String("to_uid", req.ToUID))
				respondUserRequestInvalid(c, "vercode")
				return
			}
			req.Vercode = friend.SourceVercode
			verifyVercode = false
		}
	}

	if verifyVercode {
		//验证code是否有效
		err = source.CheckRequestAddFriendCode(req.Vercode, fromUID)
		if err != nil {
			f.Warn("好友申请验证码校验失败", zap.Error(err), zap.String("uid", fromUID), zap.String("toUid", req.ToUID))
			respondUserError(c, errcode.ErrUserCodeInvalid)
			return
		}
	}

	// 提取 space_id：body > query > header（客户端可能从任意层传递）
	spaceID := req.SpaceID
	if spaceID == "" {
		spaceID = c.Query("space_id")
	}
	if spaceID == "" {
		spaceID = c.GetHeader("X-Space-ID")
	}

	// YUJ-231 / GH#1290：纵深防御——好友申请 claim 的 space_id 来自 client
	// 可控输入（body/query/header），无校验则攻击者可伪造任意 Space，让
	// 攻击者-受害者的 DM tip payload 出现在受害者的其它 Space 视图中
	// （客户端按 payload.space_id 路由）。非成员降级为空串，走默认 Space 兜底。
	// 参考：modules/group/api.go 的 YUJ-201/YUJ-219 pattern。
	if spaceID != "" {
		inSpace, membershipErr := spacepkg.CheckMembership(f.ctx.DB(), spaceID, fromUID)
		if membershipErr != nil {
			f.Error("好友申请 space_id 成员校验失败",
				zap.String("uid", fromUID),
				zap.String("spaceId", spaceID),
				zap.Error(membershipErr))
			spaceID = "" // DB 抖动时降级，不阻断主流程
		} else if !inSpace {
			f.Warn("friend apply: not a member of claimed space, dropping claim",
				zap.String("uid", fromUID),
				zap.String("spaceId", spaceID))
			spaceID = "" // 非成员降级，避免伪造 space_id 写入 DM payload
		}
	}

	// 设置token
	token := util.GenerUUID()

	err = f.ctx.Cache().SetAndExpire(f.ctx.GetConfig().Cache.FriendApplyTokenCachePrefix+token+toUser.UID, util.ToJson(map[string]interface{}{
		"from_uid": fromUID,
		"vercode":  req.Vercode,
		"remark":   req.Remark,
		"space_id": spaceID,
	}), f.ctx.GetConfig().Cache.FriendApplyExpire)
	if err != nil {
		f.Error("设置申请token失败！", zap.Error(err))
		respondUserError(c, errcode.ErrUserTokenCacheFailed)
		return
	}
	// 查询好友申请记录
	apply, err := f.db.queryApplyWithUidAndToUid(req.ToUID, fromUID)
	if err != nil {
		f.Error("查询好友申请记录错误", zap.Error(err), zap.String("to_uid", req.ToUID))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	// 查询用户红点
	userRedDot, err := f.userDB.queryUserRedDot(req.ToUID, UserRedDotCategoryFriendApply)
	if err != nil {
		f.Error("查询用户通讯录红点信息错误", zap.Error(err), zap.String("to_uid", req.ToUID))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	tx, err := f.ctx.DB().Begin()
	if err != nil {
		f.Error("开启事务失败！", zap.Error(err))
		respondUserError(c, errcode.ErrUserStoreFailed)
		return
	}
	defer func() {
		if err := recover(); err != nil {
			tx.Rollback()
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
		}
	}()
	isAddCount := false
	if apply == nil {
		err = f.db.insertApplyTx(&FriendApplyModel{
			Status: 0,
			UID:    req.ToUID,
			ToUID:  fromUID,
			Remark: req.Remark,
			Token:  token,
		}, tx)
		if err != nil {
			tx.Rollback()
			f.Error("新增好友申请记录错误", zap.Error(err), zap.String("to_uid", req.ToUID))
			respondUserError(c, errcode.ErrUserStoreFailed)
			return
		}
	} else {
		// if apply.Status != 0 {
		isAddCount = true
		apply.Status = 0
		apply.Token = token
		err = f.db.updateApplyTx(apply, tx)
		if err != nil {
			tx.Rollback()
			f.Error("修改好友申请记录错误", zap.Error(err), zap.String("to_uid", req.ToUID))
			respondUserError(c, errcode.ErrUserStoreFailed)
			return
		}
		// }

	}
	// 新增红点
	if userRedDot == nil {
		err = f.userDB.insertUserRedDotTx(&userRedDotModel{
			UID:      req.ToUID,
			Count:    1,
			IsDot:    0,
			Category: UserRedDotCategoryFriendApply,
		}, tx)
		if err != nil {
			tx.Rollback()
			f.Error("新增用户通讯录红点信息错误", zap.Error(err), zap.String("to_uid", req.ToUID))
			respondUserError(c, errcode.ErrUserStoreFailed)
			return
		}
	} else {
		if isAddCount || userRedDot.Count == 0 {
			userRedDot.Count++
			err = f.userDB.updateUserRedDotTx(userRedDot, tx)
			if err != nil {
				tx.Rollback()
				f.Error("修改用户通讯录红点信息错误", zap.Error(err), zap.String("to_uid", req.ToUID))
				respondUserError(c, errcode.ErrUserStoreFailed)
				return
			}
		}

	}
	if err = tx.Commit(); err != nil {
		tx.Rollback()
		f.Error("提交事物错误", zap.Error(err))
		respondUserError(c, errcode.ErrUserStoreFailed)
		return
	}
	// 发送消息
	cmdParam := map[string]interface{}{
		"apply_uid":  fromUID,
		"apply_name": fromName,
		"to_uid":     toUser.UID,
		"remark":     req.Remark,
		"token":      token,
	}
	if spaceID != "" {
		cmdParam["space_id"] = spaceID
	}
	err = f.ctx.SendCMD(config.MsgCMDReq{
		CMD:         common.CMDFriendRequest,
		ChannelID:   toUser.UID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Param:       cmdParam,
	})
	if err != nil {
		f.Error("发送好友申请失败！", zap.Error(err))
		respondUserError(c, errcode.ErrUserIMCallFailed)
		return
	}

	// 如果目标是机器人，检查是否自动通过
	if toUser.Robot == 1 {
		var autoApprove int
		_ = f.ctx.DB().Select("IFNULL(auto_approve,0)").From("robot").Where("robot_id=? AND status=1", toUser.UID).LoadOne(&autoApprove)
		if autoApprove == 1 {
			// 自动通过好友申请
			go f.autoApproveFriend(fromUID, toUser.UID, token, spaceID)
		} else if botFriendApplyHook != nil {
			// 需要 owner 审批，传递 space_id 保证通知隔离到正确 Space
			go botFriendApplyHook(fromUID, fromName, toUser.UID, req.Remark, token, spaceID)
		}
	}

	c.ResponseOK()
}

// 确认好友
// autoApproveFriend Bot 自动通过好友申请
func (f *Friend) autoApproveFriend(fromUID string, botUID string, token string, spaceID string) {
	// 建立双向好友关系
	tx, err := f.ctx.DB().Begin()
	if err != nil {
		f.Error("auto approve: 开启事务失败", zap.Error(err))
		return
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	version, err := f.ctx.GenSeq(common.FriendSeqKey)
	if err != nil {
		tx.Rollback()
		f.Error("auto approve: GenSeq 失败", zap.Error(err))
		return
	}

	// fromUID -> botUID（处理 is_deleted=1 的已有记录）
	existingFriend, _ := f.db.queryWithUID(fromUID, botUID)
	if existingFriend != nil {
		err = f.db.updateRelationshipTx(fromUID, botUID, 0, 0, existingFriend.SourceVercode, version, tx)
		if err != nil {
			tx.Rollback()
			f.Error("auto approve: 恢复好友关系失败", zap.Error(err))
			return
		}
	} else {
		err = f.db.InsertTx(&FriendModel{
			UID:     fromUID,
			ToUID:   botUID,
			Version: version,
		}, tx)
		if err != nil {
			f.Warn("auto approve: 添加好友关系失败", zap.Error(err))
		}
	}

	// botUID -> fromUID
	existingReverse, _ := f.db.queryWithUID(botUID, fromUID)
	if existingReverse != nil {
		err = f.db.updateRelationshipTx(botUID, fromUID, 0, 0, existingReverse.SourceVercode, version, tx)
		if err != nil {
			tx.Rollback()
			f.Error("auto approve: 恢复反向好友关系失败", zap.Error(err))
			return
		}
	} else {
		err = f.db.InsertTx(&FriendModel{
			UID:     botUID,
			ToUID:   fromUID,
			Version: version,
		}, tx)
		if err != nil {
			f.Warn("auto approve: 添加反向好友关系失败", zap.Error(err))
		}
	}

	// 更新申请状态
	apply, _ := f.db.queryApplyWithUidAndToUid(botUID, fromUID)
	if apply != nil {
		apply.Status = 1
		_ = f.db.updateApplyTx(apply, tx)
	}

	// 发布好友确认事件（触发 IM 白名单添加 + 黑名单移除）
	eventID, err := f.ctx.EventBegin(&wkevent.Data{
		Event: event.FriendSure,
		Type:  wkevent.None,
		Data: map[string]interface{}{
			"uid":    botUID,
			"to_uid": fromUID,
		},
	}, tx)
	if err != nil {
		f.Error("auto approve: 发布好友确认事件失败", zap.Error(err))
		tx.Rollback()
		return
	}

	if err = tx.Commit(); err != nil {
		tx.Rollback()
		f.Error("auto approve: 提交事务失败", zap.Error(err))
		return
	}

	f.ctx.EventCommit(eventID)

	// 发送好友确认 CMD
	acceptParam := map[string]interface{}{
		"from_uid": botUID,
		"to_uid":   fromUID,
	}
	if spaceID != "" {
		acceptParam["space_id"] = spaceID
	}
	_ = f.ctx.SendCMD(config.MsgCMDReq{
		CMD:         common.CMDFriendAccept,
		ChannelID:   fromUID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Param:       acceptParam,
	})
	f.Info("Bot auto approve friend", zap.String("bot", botUID), zap.String("user", fromUID))
}

func (f *Friend) friendSure(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	name := c.GetLoginName()
	var req sureReq
	if err := c.BindJSON(&req); err != nil {
		f.Error(common.ErrData.Error(), zap.Error(err))
		respondUserRequestInvalid(c, "")
		return
	}
	if err := req.Check(); err != nil {
		// applyReq/sureReq.Check 返回"X 不能为空"类输入校验错误，统一走
		// request_invalid（不做 per-field 标注，与 /v1/user login 一致）。
		respondUserRequestInvalid(c, "")
		return
	}
	// 提取 space_id：body > query > header
	spaceID := req.SpaceID
	if spaceID == "" {
		spaceID = c.Query("space_id")
	}
	if spaceID == "" {
		spaceID = c.GetHeader("X-Space-ID")
	}

	// YUJ-231 / GH#1290：纵深防御——friendSure claim 的 space_id 同样是
	// client 可控输入，非成员降级为空串（之后 cache fallback 可覆盖）。
	// loginUID 是当前确认者，写进 DM tip payload 的 space_id 必须是其成员
	// Space；否则攻击者可让 DM 出现在受害者任意 Space 视图。
	if spaceID != "" {
		inSpace, membershipErr := spacepkg.CheckMembership(f.ctx.DB(), spaceID, loginUID)
		if membershipErr != nil {
			f.Error("好友确认 space_id 成员校验失败",
				zap.String("uid", loginUID),
				zap.String("spaceId", spaceID),
				zap.Error(membershipErr))
			spaceID = ""
		} else if !inSpace {
			f.Warn("friend sure: not a member of claimed space, dropping claim",
				zap.String("uid", loginUID),
				zap.String("spaceId", spaceID))
			spaceID = ""
		}
	}
	key := f.ctx.GetConfig().Cache.FriendApplyTokenCachePrefix + req.Token + loginUID
	tokenVaule, err := f.ctx.Cache().Get(key) // 获取申请人的uid
	if err != nil {
		// 真正的 Redis 读取故障 → 5xx；token 过期/缺失走的是 ("", nil)，由下方
		// JsonToMap 分支按客户端错误处理。
		f.Error("获取好友申请token的信息失败！", zap.Error(err), zap.String("key", key))
		respondUserError(c, errcode.ErrUserTokenCacheFailed)
		return
	}
	valueMap, err := util.JsonToMap(tokenVaule)
	if err != nil {
		// 过期/缺失的 token 表现为空串或非法 payload —— 属客户端态（链接失效），
		// 返回 400 而非 500，避免把"好友申请已过期"误报成服务器故障。
		f.Warn("好友申请 token 无效或已过期", zap.Error(err), zap.String("key", key))
		respondUserError(c, errcode.ErrUserFriendApplyInvalid)
		return
	}

	loginUser, err := f.userDB.QueryByUID(loginUID)
	if err != nil {
		f.Error("查询用户信息失败！", zap.Error(err), zap.String("uid", loginUID))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	if loginUser == nil || loginUser.IsDestroy == IsDestroyDone {
		f.Error("当前用户不存在或已注销！", zap.String("uid", loginUID))
		respondUserError(c, errcode.ErrUserCurrentNotFound)
		return
	}

	applyUID, ok := valueMap["from_uid"].(string)
	if !ok {
		f.Error("好友申请数据无效：from_uid 类型错误或不存在", zap.Any("from_uid", valueMap["from_uid"]))
		respondUserError(c, errcode.ErrUserFriendApplyInvalid)
		return
	}
	vercode, ok := valueMap["vercode"].(string)
	if !ok {
		f.Error("好友申请数据无效：vercode 类型错误或不存在", zap.Any("vercode", valueMap["vercode"]))
		respondUserError(c, errcode.ErrUserFriendApplyInvalid)
		return
	}
	remark := ""
	if valueMap["remark"] != nil {
		if remarkVal, ok := valueMap["remark"].(string); ok {
			remark = remarkVal
		}
	}
	// 从 cache 读取申请时的 space_id 作为 fallback
	if spaceID == "" {
		if cachedSpaceID, ok := valueMap["space_id"].(string); ok {
			spaceID = cachedSpaceID
		}
	}

	applyUser, err := f.userDB.QueryByUID(applyUID)
	if err != nil {
		f.Error("查询申请人用户信息失败！", zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	if applyUser == nil || applyUser.IsDestroy == IsDestroyDone {
		f.Error("申请人不存在或已注销！", zap.String("uid", applyUID))
		respondUserError(c, errcode.ErrUserNotFound)
		return
	}
	if remark == "" {
		remark = fmt.Sprintf("我是%s", applyUser.Name)
	}
	if strings.TrimSpace(applyUID) == "" || strings.TrimSpace(vercode) == "" {
		respondUserError(c, errcode.ErrUserFriendApplyInvalid)
		return
	}
	channelServiceObj := register.GetService(ChannelServiceName)
	var channelService chservice.IService
	if channelServiceObj != nil {
		channelService = channelServiceObj.(chservice.IService)
	}
	if channelService != nil {
		if applyUser.MsgExpireSecond > 0 {
			err = channelService.CreateOrUpdateMsgAutoDelete(common.GetFakeChannelIDWith(applyUID, loginUID), common.ChannelTypePerson.Uint8(), applyUser.MsgExpireSecond)
			if err != nil {
				f.Warn("设置消息自动删除失败", zap.Error(err))
			}
		}
	}
	// 是否是好友
	applyFriendModel, err := f.db.queryWithUID(loginUID, applyUID)
	if err != nil {
		f.Error("查询是否是好友失败！", zap.Error(err), zap.String("uid", loginUID), zap.String("toUid", applyUID))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	// 添加好友到数据库
	tx, err := f.ctx.DB().Begin()
	if err != nil {
		f.Error("开启事务失败！", zap.Error(err))
		respondUserError(c, errcode.ErrUserStoreFailed)
		return
	}
	defer func() {
		if err := recover(); err != nil {
			tx.Rollback()
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
		}
	}()
	version, err := f.ctx.GenSeq(common.FriendSeqKey)
	if err != nil {
		tx.Rollback()
		f.Error("生成好友关系序列号失败", zap.Error(err))
		respondUserError(c, errcode.ErrUserStoreFailed)
		return
	}
	if applyFriendModel == nil {
		// 验证code
		err = source.CheckSource(vercode)
		if err != nil {
			tx.Rollback()
			f.Warn("好友确认验证码校验失败", zap.Error(err), zap.String("uid", loginUID), zap.String("toUid", applyUID))
			respondUserError(c, errcode.ErrUserCodeInvalid)
			return
		}

		util.CheckErr(err)
		err = f.db.InsertTx(&FriendModel{
			UID:           loginUID,
			ToUID:         applyUID,
			Version:       version,
			Initiator:     0,
			IsAlone:       0,
			Vercode:       fmt.Sprintf("%s@%d", util.GenerUUID(), common.Friend),
			SourceVercode: vercode,
		}, tx)
		if err != nil {
			tx.Rollback()
			f.Error("保存好友关系失败", zap.Error(err), zap.String("uid", loginUID), zap.String("toUid", applyUID))
			respondUserError(c, errcode.ErrUserStoreFailed)
			return
		}
	} else {
		err = f.db.updateRelationshipTx(loginUID, applyUID, 0, 0, vercode, version, tx)
		if err != nil {
			tx.Rollback()
			f.Error("保存好友关系失败", zap.Error(err), zap.String("uid", loginUID), zap.String("toUid", applyUID))
			respondUserError(c, errcode.ErrUserStoreFailed)
			return
		}
	}
	// 是否是好友
	loginFriendModel, err := f.db.queryWithUID(applyUID, loginUID)
	//loginIsFriend, err := f.db.IsFriend(applyUID, loginUID)
	if err != nil {
		tx.Rollback()
		f.Error("查询被添加者是否是好友失败！", zap.Error(err), zap.String("uid", loginUID), zap.String("toUid", applyUID))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	if loginFriendModel == nil {
		err = f.db.InsertTx(&FriendModel{
			UID:           applyUID,
			ToUID:         loginUID,
			Version:       version,
			Initiator:     1,
			IsAlone:       0,
			Vercode:       fmt.Sprintf("%s@%d", util.GenerUUID(), common.Friend),
			SourceVercode: vercode,
		}, tx)
		if err != nil {
			tx.Rollback()
			f.Error("保存好友关系失败", zap.Error(err), zap.String("uid", loginUID), zap.String("toUid", applyUID))
			respondUserError(c, errcode.ErrUserStoreFailed)
			return
		}
	} else {
		err = f.db.updateRelationshipTx(applyUID, loginUID, 0, 0, vercode, version, tx)
		if err != nil {
			tx.Rollback()
			f.Error("保存好友关系失败", zap.Error(err), zap.String("uid", loginUID), zap.String("toUid", applyUID))
			respondUserError(c, errcode.ErrUserStoreFailed)
			return
		}
	}
	// 发布好友确认事件
	eventID, err := f.ctx.EventBegin(&wkevent.Data{
		Event: event.FriendSure,
		Type:  wkevent.None,
		Data: map[string]interface{}{
			"uid":    loginUID,
			"to_uid": applyUID,
		},
	}, tx)
	if err != nil {
		f.Error("发送好友确认事件失败", zap.Error(err))
		tx.Rollback()
		respondUserError(c, errcode.ErrUserStoreFailed)
		return
	}
	// 查询好友申请记录
	apply, err := f.db.queryApplyWithUidAndToUid(loginUID, applyUID)
	if err != nil {
		f.Error("查询好友申请记录错误", zap.Error(err))
		tx.Rollback()
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	if apply != nil {
		apply.Status = 1
		err = f.db.updateApplyTx(apply, tx)
		if err != nil {
			f.Error("修改好友申请记录错误", zap.Error(err))
			tx.Rollback()
			respondUserError(c, errcode.ErrUserStoreFailed)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		f.Error("提交事务失败！", zap.Error(err))
		respondUserError(c, errcode.ErrUserStoreFailed)
		return
	}
	f.ctx.EventCommit(eventID)

	// 发送确认消息给对方
	sureCmdParam := map[string]interface{}{
		"to_uid":    applyUID,
		"from_uid":  loginUID,
		"from_name": name,
	}
	if spaceID != "" {
		sureCmdParam["space_id"] = spaceID
	}
	err = f.ctx.SendCMD(config.MsgCMDReq{
		CMD:         common.CMDFriendAccept,
		Subscribers: []string{applyUID, loginUID},
		Param:       sureCmdParam,
	})
	if err != nil {
		f.Error("发送消息失败！", zap.Error(err))
		respondUserError(c, errcode.ErrUserIMCallFailed)
		return
	}
	content := "我们已经是好友了，可以愉快的聊天了！"
	if f.ctx.GetConfig().Friend.AddedTipsText != "" {
		content = f.ctx.GetConfig().Friend.AddedTipsText
	}
	// YUJ-674 / Mininglamp-OSS#37: PERSONAL DM 用 octo-lib 的 NewPersonalMsgSendReq
	// builder 构造，把 payload.space_id 的服务端权威 / fail-closed strip 语义集中
	// 到一处。spaceID 已经通过上方 spacepkg.CheckMembership 校验，可以作为 senderSpaceID
	// 直接传入。
	tipPayloadMap := map[string]interface{}{
		"content": content,
		"type":    common.Tip,
	}
	err = f.ctx.SendMessage(config.NewPersonalMsgSendReq(
		applyUID, // channel = peer (DM target)
		loginUID, // from = current user
		tipPayloadMap,
		spaceID,
		config.PersonalMsgOptions{Header: config.MsgHeader{RedDot: 1}},
	))
	if err != nil {
		f.Error("发送通过好友请求消息失败！", zap.Error(err))
		respondUserError(c, errcode.ErrUserIMCallFailed)
		return
	}

	remarkPayloadMap := map[string]interface{}{
		"content": remark,
		"type":    common.Text,
	}
	err = f.ctx.SendMessage(config.NewPersonalMsgSendReq(
		loginUID, // channel = current user (mirror tip back to self thread)
		applyUID, // from = applicant
		remarkPayloadMap,
		spaceID,
		config.PersonalMsgOptions{Header: config.MsgHeader{RedDot: 1}},
	))
	if err != nil {
		f.Error("发送接受好友请求消息失败！", zap.Error(err))
		respondUserError(c, errcode.ErrUserIMCallFailed)
		return
	}

	err = f.ctx.Cache().Delete(key)
	if err != nil {
		f.Error("删除缓存数据错误", zap.Error(err))
		respondUserError(c, errcode.ErrUserTokenCacheFailed)
		return
	}
	c.ResponseOK()
}

// 同步好友
func (f *Friend) friendSync(c *wkhttp.Context) {
	uid := c.MustGet("uid").(string)
	limit := wkutil.ParseUint64OrDefault(c.Query("limit"), 0)
	if limit <= 0 {
		limit = 1000
	}
	version := wkutil.ParseInt64OrDefault(c.Query("version"), 0)
	apiVersion := wkutil.ParseInt64OrDefault(c.Query("api_version"), 0)
	spaceID := c.Query("space_id")

	// 老客户端兼容：没带 api_version=1 或没带 space_id 时，
	// 从 space_member 表查询默认 Space 的所有成员，伪装成好友格式返回
	if apiVersion == 0 || spaceID == "" {
		defaultSpaceID := spaceID
		if defaultSpaceID == "" {
			defaultSpaceID = space.GetUserDefaultSpaceID(f.ctx, uid)
		}
		if defaultSpaceID == "" {
			c.JSON(http.StatusOK, make([]*friendResp, 0))
			return
		}
		memberUIDs, err := space.GetSpaceMemberUIDs(f.ctx, defaultSpaceID)
		if err != nil {
			f.Error("获取 Space 成员失败！", zap.Error(err))
			respondUserError(c, errcode.ErrUserQueryFailed)
			return
		}
		// 排除自己
		filteredUIDs := make([]string, 0, len(memberUIDs))
		for _, m := range memberUIDs {
			if m != uid {
				filteredUIDs = append(filteredUIDs, m)
			}
		}
		userDetails, err := f.userService.GetUserDetails(c.Request.Context(), filteredUIDs, c.GetLoginUID())
		if err != nil {
			f.Error("获取用户详情失败！", zap.Error(err))
			respondUserError(c, errcode.ErrUserQueryFailed)
			return
		}
		resps := make([]*friendResp, 0, len(userDetails))
		for _, userDetail := range userDetails {
			resp := &friendResp{}
			resp.UserDetailResp = *userDetail
			resp.Version = 1
			resps = append(resps, resp)
		}
		c.JSON(http.StatusOK, resps)
		return
	}

	var friends []*FriendModel
	var err error
	// 同步好友
	friends, err = f.db.SyncFriends(version, uid, limit)
	if err != nil {
		f.Error("同步好友信息错误！", zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}

	friendUIDs := make([]string, 0, len(friends))
	if len(friends) > 0 {
		for _, f := range friends {
			friendUIDs = append(friendUIDs, f.ToUID)
		}
	}
	userDetails, err := f.userService.GetUserDetails(c.Request.Context(), friendUIDs, c.GetLoginUID())
	if err != nil {
		f.Error("获取用户详情失败！", zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	userDetailMap := map[string]*UserDetailResp{}
	if len(userDetails) > 0 {
		for _, userDetail := range userDetails {
			userDetailMap[userDetail.UID] = userDetail
		}
	}
	resps := make([]*friendResp, 0)
	if len(friends) > 0 {
		for _, f := range friends {
			resp := &friendResp{}
			resp.IsDeleted = f.IsDeleted
			resp.Version = f.Version
			resp.Vercode = f.Vercode
			userDetail := userDetailMap[f.ToUID]
			if userDetail != nil {
				resp.UserDetailResp = *userDetail
			}
			resps = append(resps, resp)
		}
	}
	c.JSON(http.StatusOK, resps)
}

func (f *Friend) friendSearch(c *wkhttp.Context) {
	uid := c.MustGet("uid").(string)
	keyword := c.Query("keyword")
	spaceID := c.Query("space_id")

	// Space 模式：从 space_member 表搜索，而非 friend 表
	if spaceID != "" {
		memberUIDs, err := space.GetSpaceMemberUIDs(f.ctx, spaceID)
		if err != nil {
			f.Error("获取 Space 成员失败！", zap.Error(err))
			respondUserError(c, errcode.ErrUserQueryFailed)
			return
		}
		// 排除自己
		filteredUIDs := make([]string, 0, len(memberUIDs))
		for _, m := range memberUIDs {
			if m != uid {
				filteredUIDs = append(filteredUIDs, m)
			}
		}
		userDetails, err := f.userService.GetUserDetails(c.Request.Context(), filteredUIDs, c.GetLoginUID())
		if err != nil {
			f.Error("获取用户详情失败！", zap.Error(err))
			respondUserError(c, errcode.ErrUserQueryFailed)
			return
		}
		resps := make([]*friendResp, 0, len(userDetails))
		for _, userDetail := range userDetails {
			// keyword 过滤
			if keyword != "" && !strings.Contains(strings.ToLower(userDetail.Name), strings.ToLower(keyword)) {
				continue
			}
			resp := &friendResp{}
			resp.UserDetailResp = *userDetail
			resp.Version = 1
			resps = append(resps, resp)
		}
		c.JSON(http.StatusOK, resps)
		return
	}

	// 非 Space 模式：原有 friend 表查询
	friends, err := f.db.QueryFriendsWithKeyword(uid, keyword)
	if err != nil {
		f.Error("查询好友数据失败！", zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	resps := make([]*friendResp, 0)
	if len(friends) > 0 {
		for _, f := range friends {
			resp := &friendResp{}
			blacklist := 1
			if f.Blacklist == 1 {
				blacklist = 2
			}
			resp.From(f, blacklist, 0)
			resps = append(resps, resp)
		}
	}
	c.JSON(http.StatusOK, resps)
}

// 设置好友备注
func (f *Friend) remark(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	var req remarkReq
	if err := c.BindJSON(&req); err != nil {
		f.Error(common.ErrData.Error(), zap.Error(err))
		respondUserRequestInvalid(c, "")
		return
	}
	if req.UID == "" {
		respondUserRequestInvalid(c, "uid")
		return
	}
	settingM, err := f.settingDB.querySettingByUIDAndToUID(loginUID, req.UID)
	if err != nil {
		f.Error("查询设置信息失败！", zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	if settingM == nil {
		settingM = newDefaultSettingModel()
		settingM.UID = loginUID
		settingM.ToUID = req.UID
		settingM.Remark = req.Remark
		err = f.settingDB.InsertUserSettingModel(settingM)
		if err != nil {
			f.Error("添加用户设置失败！", zap.Error(err))
			respondUserError(c, errcode.ErrUserStoreFailed)
			return
		}
	} else {
		settingM.Remark = req.Remark
		err = f.settingDB.UpdateUserSettingModel(settingM)
		if err != nil {
			f.Error("修改用户备注错误", zap.Error(err))
			respondUserError(c, errcode.ErrUserStoreFailed)
			return
		}
	}

	err = f.ctx.SendChannelUpdateToUser(loginUID, config.ChannelReq{
		ChannelID:   req.UID,
		ChannelType: common.ChannelTypePerson.Uint8(),
	})
	if err != nil {
		f.Warn("修改备注-发送频道更新消息失败", zap.Error(err))
	}
	c.ResponseOK()
}

// ---------- vo ----------
// 好友申请请求
type applyReq struct {
	ToUID   string `json:"to_uid"`   // 向谁申请好友
	Remark  string `json:"remark"`   // 备注
	Vercode string `json:"vercode"`  // 验证码
	SpaceID string `json:"space_id"` // Space ID（可选，客户端从 body 传递）
}

// 修改好友备注请求
type remarkReq struct {
	UID    string `json:"uid"`    //好友UID
	Remark string `json:"remark"` //备注名称
}

func (r applyReq) Check() error {
	if strings.TrimSpace(r.ToUID) == "" {
		return errors.New("好友的ID不能为空！")
	}
	// if strings.TrimSpace(r.Vercode) == "" {
	// 	return errors.New("验证码不能为空！")
	// }
	return nil
}

type sureReq struct {
	Token   string `json:"token"`    // 收到申请的token
	SpaceID string `json:"space_id"` // Space ID（可选，客户端从 body 传递）
}

func (r sureReq) Check() error {
	if strings.TrimSpace(r.Token) == "" {
		return errors.New("接收申请的token不能为空！")
	}
	return nil
}

type friendResp struct {
	UserDetailResp

	// ID        int64  `json:"id"`
	// ToUID     string `json:"to_uid"`
	// ToName    string `json:"to_name"`
	// ToRemark  string `json:"to_remark"`
	// Mute      int    `json:"mute"`
	// Top       int    `json:"top"`
	// Version   int64  `json:"version"`
	// CreatedAt string `json:"created_at"`
	// UpdatedAt string `json:"updated_at"`
	// IsDeleted int    `json:"is_deleted"`
	// ShortNo   string `json:"short_no"`
	// Code      string `json:"code"`
	// ChatPwdOn int    `json:"chat_pwd_on"`
	// Status    int    `json:"status"`
	// Receipt   int    `json:"receipt"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	IsDeleted int    `json:"is_deleted"`
	Version   int64  `json:"version"`
}

type friendApplyResp struct {
	Id        int64  `json:"id"`
	UID       string `json:"uid"`
	ToUID     string `json:"to_uid"`
	ToName    string `json:"to_name"`
	Remark    string `json:"remark"`
	Status    int    `json:"status"` // 状态 0.未处理 1.通过 2.拒绝
	Token     string `json:"token"`
	CreatedAt string `json:"created_at"`
}

func (f *friendResp) From(m *DetailModel, blacklist int, beBlacklist int) {
	f.UID = m.ToUID
	f.Name = m.ToName
	f.Mute = m.Mute
	f.Top = m.Top
	f.ShortNo = m.ShortNo
	f.Code = m.Vercode
	f.Vercode = m.Vercode
	f.Remark = m.Remark
	f.ChatPwdOn = m.ChatPwdOn
	f.Status = blacklist
	f.Receipt = m.Receipt
	f.Follow = 1
	f.Version = m.Version
	f.IsDeleted = m.IsDeleted
	f.Category = m.ToCategory
	f.Robot = m.Robot
	f.CreatedAt = m.CreatedAt.String()
	f.UpdatedAt = m.UpdatedAt.String()
	f.BeDeleted = m.IsAlone
	f.BeBlacklist = beBlacklist
}
