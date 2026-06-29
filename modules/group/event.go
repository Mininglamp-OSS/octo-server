package group

import (
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/pool"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-server/modules/conversation_ext"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"go.uber.org/zap"
)

// handleGroupDisbandEvent 群解散
func (g *Group) handleGroupDisbandEvent(data []byte, commit config.EventCommit) {
	var req config.MsgGroupDisband
	err := util.ReadJsonByByte(data, &req)
	if err != nil {
		g.Error("解析JSON失败！", zap.Error(err))
		commit(nil)
		return
	}
	if req.GroupNo == "" {
		// 空 groupNo 守卫：对齐 handleOrgEmployeeExit。空值会让下游清理退化为
		// 全表/无目标的危险 SQL（如 thread_member 子查询、IMDelChannel 空频道），
		// best-effort 提交后直接 return。
		g.Error("解散群groupNo不能为空", zap.Error(errors.New("解散群groupNo不能为空")))
		commit(nil)
		return
	}

	content := "{0}已解散该群聊"
	err = g.ctx.SendMessage(&config.MsgSendReq{
		Header: config.MsgHeader{
			NoPersist: 0,
			RedDot:    1,
			SyncOnce:  0, // 只同步一次
		},
		ChannelID:   req.GroupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Payload: []byte(util.ToJson(map[string]interface{}{
			"from_uid":  req.Operator,
			"from_name": req.OperatorName,
			"content":   content,
			"extra": []config.UserBaseVo{
				{
					UID:  req.Operator,
					Name: req.OperatorName,
				},
			},
			"type": common.Tip,
		})),
	})
	if err != nil {
		// YUJ-4185 P1-1：解散提示是装饰性通知，best-effort 即可。绝不能因它失败就
		// return —— 否则后续频道删除 / 子区订阅清理被跳过，群处于“半解散”状态：
		// 成员仍订阅父群 / 子区频道，继续收消息（越权读）。只记日志，继续清理。
		g.Error("发送解散群消息错误", zap.Error(err))
	}
	// 删除channel
	err = g.ctx.IMDelChannel(&config.ChannelDeleteReq{
		ChannelID:   req.GroupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
	})
	if err != nil {
		g.Error("删除IM频道失败", zap.Error(err))
		commit(err)
		return
	}
	// YUJ-4185 P1-1：群解散必须连同所有子区(CommunityTopic)频道一起销毁，否则子区的
	// IM 频道仍存活、成员订阅未摘 → 解散后成员仍能通过子区频道收/拉历史消息（越权读）。
	// 直接 IMDelChannel 每个非删除子区频道（频道删除即断所有订阅），再清成员对子区的
	// 置顶 / 会话扩展。best-effort：单个子区清理失败只记日志，不阻塞父群解散。
	threadShortIDs, threadErr := queryThreadShortIDsForCleanup(g.ctx, req.GroupNo)
	if threadErr != nil {
		g.Error("查询群子区失败（解散清理）", zap.Error(threadErr), zap.String("groupNo", req.GroupNo))
	}
	for _, shortID := range threadShortIDs {
		threadChannelID := req.GroupNo + "____" + shortID
		if delErr := g.ctx.IMDelChannel(&config.ChannelDeleteReq{
			ChannelID:   threadChannelID,
			ChannelType: common.ChannelTypeCommunityTopic.Uint8(),
		}); delErr != nil {
			g.Error("删除子区IM频道失败（解散清理）", zap.Error(delErr), zap.String("channelID", threadChannelID))
		}
		user.RemovePinnedForChannel(threadChannelID, common.ChannelTypeCommunityTopic.Uint8())
		conversation_ext.RemoveConvExtForChannel(threadChannelID, common.ChannelTypeCommunityTopic.Uint8())
	}
	// 删除该群所有子区的成员 / 个人设置行，避免解散后残留脏数据（频道已销毁，
	// 这些行不再有意义）。best-effort：失败只记日志。
	// 不 gate 在 len(threadShortIDs) > 0 上：对齐 removeUserFromGroupThreadsCleanup
	// 的“不 gate 扫残留”约定 —— 用户可能只 mute 过子区而从未 JoinThread（Issue #331），
	// 或子区已被删除（不在 queryThreadShortIDsForCleanup 的存活集合里）但 thread_member /
	// thread_setting 仍有残留行。这两条 DELETE 按 group_no 直删，把残留一并清掉。
	// req.GroupNo 非空已在函数开头守卫，子查询不会退化为全表。
	if _, delErr := g.ctx.DB().DeleteFrom("thread_member").
		Where("thread_id IN (SELECT id FROM thread WHERE group_no=?)", req.GroupNo).
		Exec(); delErr != nil {
		g.Error("删除子区成员记录失败（解散清理）", zap.Error(delErr), zap.String("groupNo", req.GroupNo))
	}
	if _, delErr := g.ctx.DB().DeleteFrom("thread_setting").
		Where("group_no=?", req.GroupNo).
		Exec(); delErr != nil {
		g.Error("删除子区个人设置失败（解散清理）", zap.Error(delErr), zap.String("groupNo", req.GroupNo))
	}
	// 清理所有用户对该群的置顶
	user.RemovePinnedForChannel(req.GroupNo, common.ChannelTypeGroup.Uint8())
	conversation_ext.RemoveConvExtForChannel(req.GroupNo, common.ChannelTypeGroup.Uint8())
	commit(nil)
}

// handleRegisterUserEvent 用户注册时加入系统群
func (g *Group) handleRegisterUserEvent(data []byte, commit config.EventCommit) {
	appconfig, _ := g.commonService.GetAppConfig()
	if appconfig != nil && appconfig.NewUserJoinSystemGroup == 0 {
		commit(nil)
		return
	}
	var req map[string]interface{}
	err := util.ReadJsonByByte(data, &req)
	if err != nil {
		g.Error("处理用户注册加入群聊参数有误")
		commit(err)
		return
	}
	uid, ok := req["uid"].(string)
	if !ok || uid == "" {
		g.Error("处理用户注册加入群聊UID类型错误或为空")
		commit(errors.New("处理用户注册加入群聊UID类型错误或为空"))
		return
	}
	//查询群聊是否存在
	groupModel, err := g.db.QueryWithGroupNo(g.ctx.GetConfig().Account.SystemGroupID)
	if err != nil {
		g.Error("查询群详情失败")
		commit(err)
		return
	}
	tx, err := g.db.session.Begin()
	if err != nil {
		g.Error("开启事物失败")
		commit(err)
		return
	}
	defer func() {
		if r := recover(); r != nil {
			tx.RollbackUnlessCommitted()
			var panicErr error
			switch x := r.(type) {
			case error:
				panicErr = x
			default:
				panicErr = fmt.Errorf("panic: %v", r)
			}
			commit(panicErr)
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", r, debug.Stack())
		}
	}()
	if groupModel == nil {
		//创建群
		version, err := g.ctx.GenSeq(common.GroupSeqKey)
		if err != nil {
			g.Error("GenSeq failed", zap.Error(err))
			tx.Rollback()
			commit(err)
			return
		}
		err = g.db.InsertTx(&Model{
			GroupNo:        g.ctx.GetConfig().Account.SystemGroupID,
			Name:           g.ctx.GetConfig().Account.SystemGroupName,
			IsNamed:        1, // 系统群有显式配置名 → 命名群（与 CreateGroup 的 is_named 语义一致）
			Creator:        g.ctx.GetConfig().Account.SystemUID,
			Status:         GroupStatusNormal,
			Version:        version,
			AllowExternal:  1, // 向后兼容：默认允许外部成员
			AllowNoMention: 1, // 向后兼容：默认允许群级免@
		}, tx)
		if err != nil {
			g.Error("创建群聊失败")
			tx.Rollback()
			commit(err)
			return
		}
		//添加创建者
		memberVersion, err := g.ctx.GenSeq(common.GroupMemberSeqKey)
		if err != nil {
			g.Error("GenSeq failed", zap.Error(err))
			tx.Rollback()
			commit(err)
			return
		}
		err = g.db.InsertMemberTx(&MemberModel{
			GroupNo: g.ctx.GetConfig().Account.SystemGroupID,
			UID:     g.ctx.GetConfig().Account.SystemUID,
			Role:    MemberRoleCreator,
			Status:  int(common.GroupMemberStatusNormal),
			Version: memberVersion,
		}, tx)
		if err != nil {
			g.Error("设置系统群创建者失败")
			tx.Rollback()
			commit(err)
			return
		}
		realMemberUids := make([]string, 0)
		realMemberUids = append(realMemberUids, g.ctx.GetConfig().Account.SystemUID)
		// 创建IM频道
		err = g.ctx.IMCreateOrUpdateChannel(&config.ChannelCreateReq{
			ChannelID:   g.ctx.GetConfig().Account.SystemGroupID,
			ChannelType: common.ChannelTypeGroup.Uint8(),
			Subscribers: realMemberUids,
		})
		if err != nil {
			g.Error("创建im频道失败")
			tx.Rollback()
			commit(err)
			return
		}
	}

	err = tx.Commit()
	if err != nil {
		g.Error("事物提交失败")
		tx.Rollback()
		commit(err)
		return
	}

	//将新注册的用户添加到系统群
	realMemberUids := make([]string, 0)
	realMemberUids = append(realMemberUids, uid)
	err = g.addMembers(realMemberUids, g.ctx.GetConfig().Account.SystemGroupID, g.ctx.GetConfig().Account.SystemUID, "系统账号")
	if err != nil {
		g.Error("添加注册账号到系统群失败！")
		commit(err)
		return
	}
	commit(nil)
}

// 处理群成员添加事件
func (g *Group) handleGroupMemberAddEvent(data []byte, commit config.EventCommit) {

	g.ctx.EventPool.Work <- &pool.Job{
		Data: data,
		JobFunc: func(id int64, data interface{}) {
			var dataBytes = data.([]byte)
			var req *config.MsgGroupMemberAddReq
			err := util.ReadJsonByByte(dataBytes, &req)
			if err != nil {
				g.Error("解析JSON失败！", zap.Error(err))
				commit(err)
				return
			}
			err = g.ctx.SendGroupMemberAdd(req)
			if err != nil {
				g.Error("发送群成员添加消息失败！", zap.Error(err))
				commit(err)
				return
			}
			commit(nil)
		},
	}
}

// 处理创建组织或部门事件
func (g *Group) handleOrgOrDeptCreateEvent(data []byte, commit config.EventCommit) {
	var req config.MsgOrgOrDeptCreateReq
	err := util.ReadJsonByByte(data, &req)
	if err != nil {
		g.Error("解析JSON失败！", zap.Error(err))
		commit(nil)
		return
	}
	groupModel, err := g.db.QueryWithGroupNo(req.GroupNo)
	if err != nil {
		g.Error("查询群详情失败")
		commit(err)
		return
	}
	tx, err := g.db.session.Begin()
	if err != nil {
		g.Error("开启事物失败")
		tx.Rollback()
		commit(err)
		return
	}
	defer func() {
		if r := recover(); r != nil {
			tx.RollbackUnlessCommitted()
			var panicErr error
			switch x := r.(type) {
			case error:
				panicErr = x
			default:
				panicErr = fmt.Errorf("panic: %v", r)
			}
			commit(panicErr)
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", r, debug.Stack())
		}
	}()
	if groupModel == nil {
		// 创建群
		version, err := g.ctx.GenSeq(common.GroupSeqKey)
		if err != nil {
			g.Error("GenSeq failed", zap.Error(err))
			tx.Rollback()
			commit(err)
			return
		}
		err = g.db.InsertTx(&Model{
			GroupNo:             req.GroupNo,
			Name:                req.Name,
			IsNamed:             1, // 组织/部门群有显式名 → 命名群（与 CreateGroup 的 is_named 语义一致）
			Creator:             req.Operator,
			Status:              GroupStatusNormal,
			Version:             version,
			Invite:              1,
			AllowViewHistoryMsg: 1,
			Category:            req.GroupCategory,
			AllowExternal:       1, // 向后兼容：默认允许外部成员
			AllowNoMention:      1, // 向后兼容：默认允许群级免@
		}, tx)
		if err != nil {
			g.Error("创建群聊失败")
			tx.Rollback()
			commit(err)
			return
		}

		//添加创建者
		memberVersion, err := g.ctx.GenSeq(common.GroupMemberSeqKey)
		if err != nil {
			g.Error("GenSeq failed", zap.Error(err))
			tx.Rollback()
			commit(err)
			return
		}
		err = g.db.InsertMemberTx(&MemberModel{
			GroupNo: req.GroupNo,
			UID:     req.Operator,
			Role:    MemberRoleCreator,
			Status:  int(common.GroupMemberStatusNormal),
			Version: memberVersion,
			Vercode: fmt.Sprintf("%s@%d", util.GenerUUID(), common.GroupMember),
		}, tx)
		if err != nil {
			g.Error("设置群创建者失败")
			tx.Rollback()
			commit(err)
			return
		}
		realMemberUids := make([]string, 0)
		if len(req.Members) > 0 {
			for _, member := range req.Members {
				realMemberUids = append(realMemberUids, member.EmployeeUid)
				memberVersion, err := g.ctx.GenSeq(common.GroupMemberSeqKey)
				if err != nil {
					g.Error("GenSeq failed", zap.Error(err))
					tx.Rollback()
					commit(err)
					return
				}
				err = g.db.InsertMemberTx(&MemberModel{
					GroupNo: req.GroupNo,
					UID:     member.EmployeeUid,
					Role:    MemberRoleCommon,
					Status:  int(common.GroupMemberStatusNormal),
					Version: memberVersion,
					Vercode: fmt.Sprintf("%s@%d", util.GenerUUID(), common.GroupMember),
				}, tx)
				if err != nil {
					g.Error("添加群成员错误")
					tx.Rollback()
					commit(err)
					return
				}
			}
		}

		realMemberUids = append(realMemberUids, req.Operator)
		// 创建IM频道
		err = g.ctx.IMCreateOrUpdateChannel(&config.ChannelCreateReq{
			ChannelID:   req.GroupNo,
			ChannelType: common.ChannelTypeGroup.Uint8(),
			Subscribers: realMemberUids,
		})
		if err != nil {
			g.Error("创建im频道失败")
			tx.Rollback()
			commit(err)
			return
		}
	}
	err = tx.Commit()
	if err != nil {
		g.Error("事物提交失败")
		tx.Rollback()
		commit(err)
		return
	}
	// 发送一条系统消息
	content := fmt.Sprintf("欢迎%s加入%s，新成员入群可查看所有历史消息", req.OperatorName, req.Name)
	err = g.ctx.SendMessage(&config.MsgSendReq{
		Header: config.MsgHeader{
			NoPersist: 0,
			RedDot:    1,
			SyncOnce:  0, // 只同步一次
		},
		ChannelID:   req.GroupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Payload: []byte(util.ToJson(map[string]interface{}{
			"from_uid":  req.Operator,
			"from_name": req.OperatorName,
			"content":   content,
			"type":      common.GroupMemberAdd,
		})),
	})
	if err != nil {
		g.Error("发送系统消息错误")
		commit(err)
		return
	}
	commit(nil)
}

// 批量处理组织或部门成员改变部门事件
func (g *Group) handleOrgOrDeptEmployeeUpdate(data []byte, commit config.EventCommit) {
	var req config.MsgOrgOrDeptEmployeeUpdateReq
	err := util.ReadJsonByByte(data, &req)
	if err != nil {
		g.Error("解析JSON失败！", zap.Error(err))
		commit(nil)
		return
	}
	if len(req.Members) == 0 {
		g.Error("数据不能为空", zap.Error(errors.New("数据不能为空")))
		commit(nil)
		return
	}
	groupNos := make([]string, 0)
	for _, m := range req.Members {
		groupNos = append(groupNos, m.GroupNo)
	}
	groups, err := g.db.QueryGroupsWithGroupNos(groupNos)
	if err != nil {
		g.Error("批量查询群信息错误")
		commit(err)
		return
	}
	// 真实存在的群聊
	realList := make([]*config.OrgOrDeptEmployeeVO, 0)
	for _, m := range req.Members {
		isAdd := false
		for _, g := range groups {
			if m.GroupNo == g.GroupNo {
				isAdd = true
				break
			}
		}
		if isAdd {
			realList = append(realList, &config.OrgOrDeptEmployeeVO{
				Operator:     m.Operator,
				OperatorName: m.OperatorName,
				EmployeeUid:  m.EmployeeUid,
				EmployeeName: m.EmployeeName,
				GroupNo:      m.GroupNo,
				Action:       m.Action,
			})
		}
	}
	type tempVO struct {
		Operator     string
		OperatorName string
		EmployeeUid  string
		EmployeeName string
		Action       string
	}
	// 通过群编号分组
	list := make(map[string][]*tempVO, 0)
	for _, m := range realList {
		tempDatas := list[m.GroupNo]
		if len(tempDatas) == 0 {
			tempDatas = make([]*tempVO, 0)
		}
		tempDatas = append(tempDatas, &tempVO{
			Operator:     m.Operator,
			OperatorName: m.OperatorName,
			EmployeeUid:  m.EmployeeUid,
			EmployeeName: m.EmployeeName,
			Action:       m.Action,
		})
		list[m.GroupNo] = tempDatas
	}
	tx, err := g.db.session.Begin()
	if err != nil {
		g.Error("开启事物失败")
		tx.Rollback()
		commit(err)
		return
	}
	defer func() {
		if r := recover(); r != nil {
			tx.RollbackUnlessCommitted()
			var panicErr error
			switch x := r.(type) {
			case error:
				panicErr = x
			default:
				panicErr = fmt.Errorf("panic: %v", r)
			}
			commit(panicErr)
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", r, debug.Stack())
		}
	}()

	// 添加或修改群成员
	for groupNo, members := range list {
		for _, member := range members {
			version, err := g.ctx.GenSeq(common.GroupMemberSeqKey)
			if err != nil {
				g.Error("GenSeq failed", zap.Error(err))
				tx.Rollback()
				commit(err)
				return
			}
			existDelete, err := g.db.ExistMemberDelete(member.EmployeeUid, groupNo)
			if err != nil {
				g.Error("查询是否存在删除成员失败！", zap.Error(err))
				tx.Rollback()
				commit(err)
				return
			}
			if member.Action == "add" {
				newMember := &MemberModel{
					GroupNo:   groupNo,
					InviteUID: member.Operator,
					UID:       member.EmployeeUid,
					Vercode:   fmt.Sprintf("%s@%d", util.GenerUUID(), common.GroupMember),
					Version:   version,
					Status:    int(common.GroupMemberStatusNormal),
					Robot:     0,
				}
				if existDelete {
					err = g.db.recoverMemberTx(newMember, tx)
				} else {
					err = g.db.InsertMemberTx(newMember, tx)
				}
				if err != nil {
					g.Error("添加群成员失败！", zap.Error(err))
					tx.Rollback()
					commit(err)
					return
				}
			} else {
				// 删除
				err = g.db.DeleteMemberTx(groupNo, member.EmployeeUid, version, tx)
				if err != nil {
					g.Error("删除群成员失败！", zap.Error(err))
					tx.Rollback()
					commit(err)
					return
				}
			}
		}
	}

	// 发布事件
	type tempMsgVO struct {
		GroupNo string
		Members []*tempVO
	}
	addMembers := make([]*tempMsgVO, 0)
	deleteMembers := make([]*tempMsgVO, 0)
	for groupNo, members := range list {
		tempList := make([]*tempVO, 0)
		for _, member := range members {
			tempList = append(tempList, &tempVO{
				Operator:     member.Operator,
				OperatorName: member.OperatorName,
				EmployeeUid:  member.EmployeeUid,
				EmployeeName: member.EmployeeName,
			})
			if member.Action == "add" {
				addMembers = append(addMembers, &tempMsgVO{
					GroupNo: groupNo,
					Members: tempList,
				})
			} else {
				deleteMembers = append(deleteMembers, &tempMsgVO{
					GroupNo: groupNo,
					Members: tempList,
				})
			}
		}
	}
	if err = tx.Commit(); err != nil {
		g.Error("事物提交失败")
		tx.Rollback()
		commit(err)
		return
	}
	// 添加IM订阅者和发布入群消息（必须在tx.Commit()成功之后）
	for _, m := range addMembers {
		groupName := ""
		for _, group := range groups {
			if m.GroupNo == group.GroupNo {
				groupName = group.Name
				break
			}
		}
		uids := make([]string, 0)
		members := make([]*config.UserBaseVo, 0)
		params := make([]string, 0, len(m.Members))
		for index := range m.Members {
			params = append(params, fmt.Sprintf("{%d}", index))
			members = append(members, &config.UserBaseVo{
				UID:  m.Members[index].EmployeeUid,
				Name: m.Members[index].EmployeeName,
			})
			uids = append(uids, m.Members[index].EmployeeUid)
		}
		err = g.ctx.IMAddSubscriber(&config.SubscriberAddReq{
			ChannelID:   m.GroupNo,
			ChannelType: common.ChannelTypeGroup.Uint8(),
			Subscribers: uids,
		})
		if err != nil {
			g.Error("调用IM的订阅接口失败！", zap.Error(err))
			commit(err)
			return
		}
		content := fmt.Sprintf("欢迎%s 加入 %s，新成员入群可查看所有历史消息", strings.Join(params, ","), groupName)
		err = g.ctx.SendMessage(&config.MsgSendReq{
			Header: config.MsgHeader{
				NoPersist: 0,
				RedDot:    1,
				SyncOnce:  0, // 只同步一次
			},
			ChannelID:   m.GroupNo,
			ChannelType: common.ChannelTypeGroup.Uint8(),
			Payload: []byte(util.ToJson(map[string]interface{}{
				// "from_uid":  operator,
				// "from_name": operatorName,
				"content": content,
				"extra":   members,
				"type":    common.GroupMemberAdd,
			})),
		})
		if err != nil {
			g.Error("发送新增组织或部门群成员消息错误", zap.Error(err))
			commit(nil)
			return
		}
	}
	// 同步新成员到群内所有子区的 IM 订阅（允许发消息）
	for _, m := range addMembers {
		uids := make([]string, 0, len(m.Members))
		for _, member := range m.Members {
			uids = append(uids, member.EmployeeUid)
		}
		g.addUsersToGroupThreads(m.GroupNo, uids)
	}

	if len(deleteMembers) > 0 {
		// YUJ-4185 P1-2：组织/部门结构更新删人也要摘子区订阅，与已修的
		// handleOrgEmployeeExit / 踢人 / 退群 对称。按 groupNo 取 SpaceID 供 helper
		// 清理 Space 维度的 pinned / 会话扩展。
		spaceIDByGroupNo := make(map[string]string, len(groups))
		for _, grp := range groups {
			spaceIDByGroupNo[grp.GroupNo] = grp.SpaceID
		}
		for _, m := range deleteMembers {
			members := make([]string, 0)
			for index := range m.Members {
				members = append(members, m.Members[index].EmployeeUid)
			}
			err = g.ctx.IMRemoveSubscriber(&config.SubscriberRemoveReq{
				ChannelID:   m.GroupNo,
				ChannelType: common.ChannelTypeGroup.Uint8(),
				Subscribers: members,
			})
			if err != nil {
				g.Error("调用IM的订阅接口失败！", zap.Error(err))
				commit(err)
				return
			}
			// Issue #27 同型：组织/部门删人必须摘除该 uid 在群内所有非删除子区的
			// IM 订阅（复用统一 helper，best-effort）。
			for _, uid := range members {
				g.removeUserFromGroupThreads(m.GroupNo, uid, spaceIDByGroupNo[m.GroupNo])
			}
			// 发送群成员更新命令
			err = g.ctx.SendCMD(config.MsgCMDReq{
				ChannelID:   m.GroupNo,
				ChannelType: common.ChannelTypeGroup.Uint8(),
				CMD:         common.CMDGroupMemberUpdate,
				Param: map[string]interface{}{
					"group_no": m.GroupNo,
				},
			})
			if err != nil {
				g.Error("发送更新群成员cmd消息错误", zap.Error(err))
				commit(err)
				return
			}
		}
	}
	commit(nil)
}

// 处理发送新增部门或组织群成员消息
// func (g *Group) handleOrgOrDeptEmployeeAddMsg(data []byte, commit config.EventCommit) {
// 	var req config.MsgOrgOrDeptEmployeeAddReq
// 	err := util.ReadJsonByByte(data, &req)
// 	if err != nil {
// 		g.Error("解析JSON失败！", zap.Error(err))
// 		commit(nil)
// 		return
// 	}
// 	if req.GroupNo == "" {
// 		g.Error("群编号不能为空", zap.Error(errors.New("群编号不能为空")))
// 		commit(nil)
// 		return
// 	}
// 	if len(req.Members) == 0 {
// 		g.Error("新增成员列表不能为空", zap.Error(errors.New("新增成员列表不能为空")))
// 		commit(nil)
// 		return
// 	}
// 	members := make([]*config.UserBaseVo, 0)
// 	params := make([]string, 0, len(req.Members))
// 	for index := range req.Members {
// 		params = append(params, fmt.Sprintf("{%d}", index))
// 		members = append(members, &config.UserBaseVo{
// 			UID:  req.Members[index].UID,
// 			Name: req.Members[index].Name,
// 		})
// 	}
// 	content := fmt.Sprintf("欢迎%s 加入 %s，新成员入群可查看所有历史消息", strings.Join(params, ","), req.Name)
// 	err = g.ctx.SendMessage(&config.MsgSendReq{
// 		Header: config.MsgHeader{
// 			NoPersist: 0,
// 			RedDot:    1,
// 			SyncOnce:  0, // 只同步一次
// 		},
// 		ChannelID:   req.GroupNo,
// 		ChannelType: common.ChannelTypeGroup.Uint8(),
// 		Payload: []byte(util.ToJson(map[string]interface{}{
// 			// "from_uid":  operator,
// 			// "from_name": operatorName,
// 			"content": content,
// 			"extra":   members,
// 			"type":    common.GroupMemberAdd,
// 		})),
// 	})
// 	if err != nil {
// 		g.Error("发送新增组织或部门群成员消息错误", zap.Error(err))
// 		commit(nil)
// 		return
// 	}
// 	commit(nil)
// }

// 处理组织成员退出
func (g *Group) handleOrgEmployeeExit(data []byte, commit config.EventCommit) {
	var req config.OrgEmployeeExitReq
	err := util.ReadJsonByByte(data, &req)
	if err != nil {
		g.Error("解析JSON失败！", zap.Error(err))
		commit(nil)
		return
	}
	if req.Operator == "" {
		g.Error("退出用户uid不能为空", zap.Error(errors.New("退出用户uid不能为空")))
		commit(nil)
		return
	}
	if len(req.GroupNos) == 0 {
		g.Error("退出群列表不能为空", zap.Error(errors.New("退出群列表不能为空")))
		commit(nil)
		return
	}
	groups, err := g.db.QueryGroupsWithGroupNos(req.GroupNos)
	if err != nil {
		g.Error("查询群列表错误", zap.Error(err))
		commit(nil)
		return
	}
	if len(groups) == 0 {
		g.Error("所在群里不存在", zap.Error(errors.New("所在群里不存在")))
		commit(nil)
		return
	}
	realGroups := make([]string, 0)
	spaceIDByGroupNo := make(map[string]string, len(groups))
	for _, groupNo := range req.GroupNos {
		for _, group := range groups {
			if groupNo == group.GroupNo {
				realGroups = append(realGroups, groupNo)
				spaceIDByGroupNo[groupNo] = group.SpaceID
				break
			}
		}
	}

	tx, err := g.db.session.Begin()
	if err != nil {
		g.Error("开启事物失败")
		tx.Rollback()
		commit(err)
		return
	}
	defer func() {
		if r := recover(); r != nil {
			tx.RollbackUnlessCommitted()
			var panicErr error
			switch x := r.(type) {
			case error:
				panicErr = x
			default:
				panicErr = fmt.Errorf("panic: %v", r)
			}
			commit(panicErr)
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", r, debug.Stack())
		}
	}()
	for _, groupNo := range realGroups {
		version, err := g.ctx.GenSeq(common.GroupMemberSeqKey)
		if err != nil {
			g.Error("GenSeq failed", zap.Error(err))
			tx.Rollback()
			commit(err)
			return
		}
		err = g.db.DeleteMemberTx(groupNo, req.Operator, version, tx)
		if err != nil {
			g.Error("删除群成员失败！", zap.Error(err))
			tx.Rollback()
			commit(err)
			return
		}
	}
	err = tx.Commit()
	if err != nil {
		g.Error("提交事物错误", zap.Error(err))
		tx.Rollback()
		commit(err)
		return
	}
	for _, groupNo := range realGroups {
		members := make([]string, 0)
		members = append(members, req.Operator)
		err = g.ctx.IMRemoveSubscriber(&config.SubscriberRemoveReq{
			ChannelID:   groupNo,
			ChannelType: common.ChannelTypeGroup.Uint8(),
			Subscribers: members,
		})
		if err != nil {
			g.Error("调用IM的订阅接口失败！", zap.Error(err))
			commit(err)
			return
		}
		// Issue #27 同型：组织退出也必须摘除该用户在群内所有非删除子区的
		// IM 订阅，与踢人/退群路径对齐（helper 为 best-effort，失败只记日志）。
		g.removeUserFromGroupThreads(groupNo, req.Operator, spaceIDByGroupNo[groupNo])
		// 发送群成员更新命令
		err = g.ctx.SendCMD(config.MsgCMDReq{
			ChannelID:   groupNo,
			ChannelType: common.ChannelTypeGroup.Uint8(),
			CMD:         common.CMDGroupMemberUpdate,
			Param: map[string]interface{}{
				"group_no": groupNo,
			},
		})
		if err != nil {
			g.Error("发送更新群成员cmd消息错误", zap.Error(err))
			commit(err)
			return
		}
	}
	commit(nil)
}
