package group

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkevent"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/base/event"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// 群邀请添加
func (g *Group) groupMemberInviteAdd(c *wkhttp.Context) {
	loginUID := c.MustGet("uid").(string)
	loginName := c.MustGet("name").(string)
	groupNo := c.Param("group_no")
	var req InviteReq
	if err := c.BindJSON(&req); err != nil {
		g.Error("数据格式有误！", zap.Error(err))
		respondGroupRequestInvalid(c, "")
		return
	}
	if err := req.Check(); err != nil {
		respondGroupRequestInvalid(c, "")
		return
	}

	_, err := g.getGroupInfo(groupNo)
	if err != nil {
		respondGroupInfoError(c, err)
		return
	}

	// Bot Ownership 预校验（YUJ-46 / issue #1181）：在邀请落库之前就拒绝
	// 非 creator 邀请别人的 bot，避免产生一条"等待管理员审批"的无效邀请。
	// 真正的兜底在 addMembersTx 里，这里只是为了更友好的错误提示。
	if botErr := checkBotOwnership(g.ctx.DB(), loginUID, req.UIDS); botErr != nil {
		if errors.Is(botErr, ErrBotOwnershipDenied) {
			httperr.ResponseErrorL(c, errcode.ErrGroupBotOwnershipDenied, nil, nil)
			return
		}
		g.Error("检查 Bot 归属失败", zap.Error(botErr))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}

	creatorOrManagerUIDS, err := g.db.QueryGroupManagerOrCreatorUIDS(groupNo)
	if err != nil {
		g.Error("查询创建者或管理员的uid失败！", zap.String("group_no", groupNo), zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}

	inviteNo := util.GenerUUID()

	tx, err := g.db.session.Begin()
	if err != nil {
		g.Error("开启事务失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	defer func() {
		if err := recover(); err != nil {
			tx.RollbackUnlessCommitted()
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
		}
	}()
	eventID, err := g.ctx.EventBegin(&wkevent.Data{
		Event: event.GroupMemberInviteRequest,
		Type:  wkevent.Message,
		Data: config.MsgGroupMemberInviteReq{
			GroupNo:     groupNo,
			InviteNo:    inviteNo,
			Inviter:     loginUID,
			InviterName: loginName,
			Num:         len(req.UIDS),
			Subscribers: creatorOrManagerUIDS,
		},
	}, tx)
	if err != nil {
		tx.Rollback()
		g.Error("开启事件失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	inviteModel := &InviteModel{
		InviteNo: inviteNo,
		GroupNo:  groupNo,
		Inviter:  loginUID,
		Remark:   req.Remark,
		Status:   InviteStatusWait,
	}
	err = g.db.InsertInviteTx(inviteModel, tx)
	if err != nil {
		tx.Rollback()
		g.Error("添加邀请数据失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	for _, uid := range req.UIDS {
		item := &InviteItemModel{
			InviteNo: inviteNo,
			GroupNo:  groupNo,
			Inviter:  loginUID,
			UID:      uid,
			Status:   InviteStatusWait,
		}
		err := g.db.InsertInviteItemTx(item, tx)
		if err != nil {
			tx.Rollback()
			g.Error("添加邀请项失败！", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		tx.Rollback()
		g.Error("提交事务失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	g.ctx.EventCommit(eventID)
	c.ResponseOK()
}

// 获取群成员邀请详情的h5
func (g *Group) getToGroupMemberConfirmInviteDetailH5(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	inviteNo := c.Query("invite_no")
	loginUID := c.MustGet("uid").(string)
	if groupNo == "" {
		respondGroupRequestInvalid(c, "group_no")
		return
	}
	_, err := g.getGroupInfo(groupNo)
	if err != nil {
		respondGroupInfoError(c, err)
		return
	}

	managerOrCreator, err := g.db.QueryIsGroupManagerOrCreator(groupNo, loginUID)
	if err != nil {
		g.Error("查询是否管理者或创建者失败！")
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if !managerOrCreator {
		httperr.ResponseErrorL(c, errcode.ErrGroupCreatorOrManagerOnly, nil, nil)
		return
	}
	authCode := util.GenerUUID()
	err = g.ctx.GetRedisConn().SetAndExpire(fmt.Sprintf("%s%s", common.AuthCodeCachePrefix, authCode), util.ToJson(map[string]interface{}{
		"group_no":  groupNo,  // 群编号
		"invite_no": inviteNo, // 邀请编号
		"allower":   loginUID, // 通过者
		"type":      common.AuthCodeTypeGroupMemberInvite,
	}), time.Minute*5)
	if err != nil {
		g.Error("缓存授权码失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}

	h5URL := fmt.Sprintf("%s/invite_detail.html?invite_no=%s&auth_code=%s", g.ctx.GetConfig().External.H5BaseURL, inviteNo, authCode)
	c.JSON(http.StatusOK, gin.H{
		"url": h5URL,
	})
}

// 群邀请确认
func (g *Group) groupMemberInviteSure(c *wkhttp.Context) {
	authCode := c.Query("auth_code")
	authInfo, err := g.ctx.GetRedisConn().GetString(fmt.Sprintf("%s%s", common.AuthCodeCachePrefix, authCode))
	if err != nil {
		g.Error("获取授权信息失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	// 空串 = auth_code 不存在 / 已过期（30min TTL），是正常用户态而非解码失败；
	// 必须在 decode 前拦截，否则会落到下面的 store_failed 内部错误分支。
	if authInfo == "" {
		httperr.ResponseErrorL(c, errcode.ErrGroupAuthCodeInvalid, nil, nil)
		return
	}
	var authMap map[string]interface{}
	err = util.ReadJsonByByte([]byte(authInfo), &authMap)
	if err != nil {
		g.Error("解码认证信息的JSON数据失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	authType, ok := authMap["type"].(string)
	if !ok {
		httperr.ResponseErrorL(c, errcode.ErrGroupAuthCodeInvalid, nil, nil)
		return
	}
	if authType != string(common.AuthCodeTypeGroupMemberInvite) {
		httperr.ResponseErrorL(c, errcode.ErrGroupAuthCodeInvalid, nil, nil)
		return
	}
	inviteNo, ok := authMap["invite_no"].(string)
	if !ok {
		httperr.ResponseErrorL(c, errcode.ErrGroupAuthCodeInvalid, nil, nil)
		return
	}
	allower, ok := authMap["allower"].(string)
	if !ok {
		httperr.ResponseErrorL(c, errcode.ErrGroupAuthCodeInvalid, nil, nil)
		return
	}
	/**
	开启事务，在事务内用 FOR UPDATE 锁定邀请记录防止并发重复处理
	**/
	tx, err := g.ctx.DB().Begin()
	if err != nil {
		g.Error("开启事务失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	defer func() {
		if err := recover(); err != nil {
			tx.Rollback()
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
		}
	}()
	/**
	判断邀请信息是否有效（事务内 FOR UPDATE 锁定）
	**/
	inviteDetailModel, err := g.db.QueryInviteDetailForUpdateTx(inviteNo, tx)
	if err != nil {
		tx.Rollback()
		g.Error("查询邀请详情失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if inviteDetailModel == nil {
		tx.Rollback()
		httperr.ResponseErrorL(c, errcode.ErrGroupInviteNotFound, nil, nil)
		return
	}
	if inviteDetailModel.Status != InviteStatusWait {
		tx.Rollback()
		httperr.ResponseErrorL(c, errcode.ErrGroupInviteStatusInvalid, nil, nil)
		return
	}
	/**
	查询邀请成员详情
	**/
	inviteItemDetilModels, err := g.db.QueryInviteItemDetail(inviteNo)
	if err != nil {
		tx.Rollback()
		g.Error("查询邀请详情失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if inviteItemDetilModels == nil || len(inviteItemDetilModels) <= 0 {
		tx.Rollback()
		httperr.ResponseErrorL(c, errcode.ErrGroupInviteNotFound, nil, nil)
		return
	}
	members := make([]string, 0, len(inviteItemDetilModels))
	groupNo := inviteItemDetilModels[0].GroupNo
	inviter := inviteItemDetilModels[0].Inviter
	for _, inviteItemDetilModel := range inviteItemDetilModels {
		members = append(members, inviteItemDetilModel.UID)
	}
	if groupNo == "" {
		tx.Rollback()
		respondGroupRequestInvalid(c, "group_no")
		return
	}
	_, err = g.getGroupInfo(groupNo)
	if err != nil {
		tx.Rollback()
		respondGroupInfoError(c, err)
		return
	}
	/**
	添加成员
	**/
	inviterUser, err := g.userDB.QueryByUID(inviter)
	if err != nil {
		tx.Rollback()
		g.Error("查询邀请者的用户信息失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if inviterUser == nil {
		tx.Rollback()
		g.Error("没有查到邀请者的用户信息！")
		httperr.ResponseErrorL(c, errcode.ErrGroupInviteNotFound, nil, nil)
		return
	}
	err = g.db.UpdateInviteStatusTx(allower, InviteStatusOK, inviteNo, tx)
	if err != nil {
		tx.Rollback()
		g.Error("更新邀请信息状态失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	err = g.db.UpdateInviteItemStatusTx(InviteStatusOK, inviteNo, tx)
	if err != nil {
		tx.Rollback()
		g.Error("更新邀请信息项状态失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	// YUJ-199 / GH#1265：邀请确认调用方携带的 X-Space-ID 才是邀请发起
	// 时所在的 Space（三端 header 拦截器注入，YUJ-88/GH#1038/EP3）。
	// 透传到 addMembersTxWithSpace，source_space_id 才能正确落点；
	// 空时 addMembersTxWithSpace 内部会走 operator / home Space 兜底。
	inviterSpaceID := strings.TrimSpace(c.GetHeader("X-Space-ID"))
	commitCallback, err := g.addMembersTxWithSpace(members, groupNo, inviterUser.UID, inviterUser.Name, inviterSpaceID, tx)
	if err != nil {
		tx.Rollback()
		g.Error("添加成员失败！", zap.Error(err))
		// 透出 allow_external 等策略拒绝的具体错误，方便管理员定位；其他底层错误走兜底文案
		if strings.Contains(err.Error(), "禁止外部成员") {
			httperr.ResponseErrorL(c, errcode.ErrGroupExternalJoinForbidden, nil, nil)
		} else {
			httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		}
		return
	}
	if err := tx.Commit(); err != nil {
		tx.Rollback()
		g.Error("提交事务失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}

	commitCallback()
	c.ResponseOK()

}

// groupMemberInviteDetail 获取群成员邀请详情
func (g *Group) groupMemberInviteDetail(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	if loginUID == "" {
		respondGroupNotLoggedIn(c)
		return
	}
	inviteNo := c.Param("invite_no")
	inviteDetilModel, err := g.db.QueryInviteDetail(inviteNo)
	if err != nil {
		g.Error("查询邀请详情失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if inviteDetilModel == nil {
		httperr.ResponseErrorL(c, errcode.ErrGroupInviteNotFound, nil, nil)
		return
	}
	inviteItems, err := g.db.QueryInviteItemDetail(inviteNo)
	if err != nil {
		g.Error("获取邀请项失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}

	// 验证请求者是否有权查看该邀请（邀请者或被邀请者）
	isAuthorized := inviteDetilModel.Inviter == loginUID
	if !isAuthorized {
		for _, item := range inviteItems {
			if item.UID == loginUID {
				isAuthorized = true
				break
			}
		}
	}
	if !isAuthorized {
		// 检查是否为群管理员或群主
		isManager, err := g.db.QueryIsGroupManagerOrCreator(inviteDetilModel.GroupNo, loginUID)
		if err != nil {
			g.Error("查询管理员信息失败！", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
			return
		}
		isAuthorized = isManager
	}
	if !isAuthorized {
		httperr.ResponseErrorL(c, errcode.ErrGroupViewForbidden, nil, nil)
		return
	}

	var uids = make([]string, 0, len(inviteItems))
	for _, item := range inviteItems {
		uids = append(uids, item.UID)
	}
	// 查询被邀请的成员
	members, err := g.db.QueryMembersWithUids(uids, inviteDetilModel.GroupNo)
	if err != nil {
		g.Error("查询成员信息错误", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	if len(members) == len(inviteItems) {
		inviteDetilModel.Status = 1
	}

	g.Debug("inviteItems-", zap.Int("len", len(inviteItems)))
	resp := InviteDetailResp{}.From(inviteDetilModel, inviteItems)
	// YUJ-168 / GH #1243: 给 H5 邀请详情 landing 补上 Space 信任锚点字段。
	// 取 invite 对应群的 space_id → space.name，再按 loginUID 计算相对外部性。
	// 查询失败不阻塞主响应（保持原有 H5 预览可用），仅字段缺省。
	if groupModel, gErr := g.db.QueryWithGroupNo(inviteDetilModel.GroupNo); gErr == nil && groupModel != nil {
		spaceName, _ := spacepkg.GetSpaceName(g.ctx.DB(), groupModel.SpaceID)
		resp.SpaceName = spaceName
		if groupModel.SpaceID != "" && loginUID != "" {
			inSpace, checkErr := spacepkg.CheckMembership(g.ctx.DB(), groupModel.SpaceID, loginUID)
			if checkErr != nil {
				g.Warn("检查 Space 成员失败", zap.Error(checkErr), zap.String("group_no", inviteDetilModel.GroupNo))
			} else if !inSpace {
				resp.IsExternal = 1
			}
		}
	} else if gErr != nil {
		g.Warn("查询群资料失败（不影响 invite 主响应）", zap.Error(gErr), zap.String("group_no", inviteDetilModel.GroupNo))
	}
	c.Response(resp)
}

// InviteReq 群邀请
type InviteReq struct {
	UIDS   []string `json:"uids"`
	Remark string   `json:"remark"`
}

// Check Check
func (i InviteReq) Check() error {
	if len(i.UIDS) <= 0 {
		return errors.New("被邀请者不能为空！")
	}
	return nil
}

// InviteDetailResp 邀请详情返回
type InviteDetailResp struct {
	InviteNo    string                 `json:"invite_no"`    // 邀请唯一编号
	GroupNo     string                 `json:"group_no"`     // 群唯一编号
	Inviter     string                 `json:"inviter"`      // 邀请者
	Remark      string                 `json:"remark"`       // 邀请备注
	InviterName string                 `json:"inviter_name"` // 邀请者名称
	Status      int                    `json:"status"`       // 状态 0.未确认 1.已确认
	Items       []InviteItemDetailResp `json:"items"`        // 邀请项详情
	// YUJ-168 / GH #1243: 外部群 H5 邀请 landing 的信任锚点字段。
	// SpaceName 始终下发（前端判断非空才渲染"来自 xx"），
	// IsExternal 仅在当前登录用户不属于该 Space 时置 1。
	SpaceName  string `json:"space_name"`  // 群所属 Space 名称（空字符串表示无 Space）
	IsExternal int    `json:"is_external"` // 访问者视角：0=内部/未登录，1=跨 Space 外部访问者
}

// From From
func (i InviteDetailResp) From(model *InviteDetailModel, items []*InviteItemDetailModel) InviteDetailResp {
	resp := InviteDetailResp{}
	resp.InviteNo = model.InviteNo
	resp.GroupNo = model.GroupNo
	resp.Inviter = model.Inviter
	resp.Remark = model.Remark
	resp.InviterName = model.InviterName
	resp.Status = model.Status
	if len(items) > 0 {
		itemResps := make([]InviteItemDetailResp, 0, len(items))
		for _, item := range items {
			itemResps = append(itemResps, InviteItemDetailResp{
				UID:  item.UID,
				Name: item.Name,
			})
		}
		resp.Items = itemResps
	}
	return resp
}

// InviteItemDetailResp 邀请item
type InviteItemDetailResp struct {
	UID  string `json:"uid"`  // 被邀请uid
	Name string `json:"name"` // 被邀请者名称
}
