package group

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
	"go.uber.org/zap"
)

// D7 —— 全员群受保护的四个操作。
//
// 全员群的成员集合**等于**项目的成员集合（不变量 I4）。所以群面上那四个会改变
// 成员集合或群归属的操作，各自都有一个项目侧的等价物，必须走那边：
//
//	群面动作            → 项目侧等价物
//	退群 exit           → 退出项目
//	踢人 members 删除   → 从项目移除成员
//	转让群主 transfer   → 转让项目 owner
//	解散群 disband      → 解散项目（没有别的等价物：群随项目结束而结束）
//
// 不挡住它们，"全员"这个词在第一个群主点解散之后就不成立了，而 I4 的对账扫描会
// 立刻开始报一个谁也修不好的违规。
//
// # 只挡 HTTP handler，绝不挡服务层
//
// 这一条是本文件里最重要的约束。RemoveGroupMembers、UpdateMemberRoleTx、
// UpdateStatusTx 这些服务层原语被四类调用方使用：
//
//   - P1 的项目成员移除级联（把离开项目的人清出项目群）；
//   - Space 成员移除级联；
//   - BotFather 删除 Bot；
//   - P2 自己的群主同步钩子（D6）。
//
// 把保护放进服务层会把这四条全部挡掉——也就是说，为了维护 I4 而加的守卫，会先把
// 维护 I2 和 I4 的那些级联干掉。保护属于"人在客户端上点了这个按钮"这一层，而不属于
// "系统正在维护不变量"那一层。
//
// # 每次判定只对项目群发起一次点查
//
// projectID 为空串（Space 直属群）时零查询直接放行，这是硬要求不是优化：这四个
// 接口承载着产品的常规流量，一个"跑了并且通过"的守卫仍然是每一次退群、每一次踢人
// 上的延迟回归。与 P1 准入闸门的 C1 是同一条纪律。

// 被拒动作，进 details.action。低基数枚举，客户端据此渲染"请到项目里操作"。
const (
	allMemberGroupActionDisband  = "disband"
	allMemberGroupActionExit     = "exit"
	allMemberGroupActionRemove   = "remove"
	allMemberGroupActionTransfer = "transfer"
)

// refuseIfAllMemberGroup 在 groupModel 是某项目的全员群时写出拒绝响应并返回 true。
//
// 调用方拿到 true 就必须立刻 return——响应已经写了。
//
// 查询失败时**放行**（返回 false）并记 Error。这是有意的取舍：这道守卫保护的是
// 产品语义（"全员群的成员由项目决定"），不是安全边界——没有它，最坏的结果是一个
// 用户退出了本该由项目管理的群，而 I4 扫描 B 会把这个缺口报出来，管理员可以把人
// 加回去。反过来，fail-closed 会让一次数据库抖动变成"所有项目群都退不出去"，
// 而那是一个用户无法自助绕过的死锁。可恢复的错误 vs 不可恢复的僵局，选前者。
func (g *Group) refuseIfAllMemberGroup(c *wkhttp.Context, groupModel *Model, action string) bool {
	if groupModel == nil || groupModel.ProjectID == "" {
		// C1：Space 直属群一次查询都不发。
		return false
	}
	isAllMember, err := projectpkg.IsAllMemberGroup(g.ctx.DB(), groupModel.ProjectID, groupModel.GroupNo)
	if err != nil {
		g.Error("判定是否为项目全员群失败，放行本次操作（见 all_member_group_guard.go 的 fail-open 论证）",
			zap.Error(err), zap.String("groupNo", groupModel.GroupNo),
			zap.String("projectId", groupModel.ProjectID), zap.String("action", action))
		return false
	}
	if !isAllMember {
		return false
	}
	g.Warn("拒绝对项目全员群的群面操作，请走项目侧入口",
		zap.String("groupNo", groupModel.GroupNo),
		zap.String("projectId", groupModel.ProjectID),
		zap.String("action", action))
	respondAllMemberGroupProtected(c, action)
	return true
}

// refuseIfAllMemberGroupByNo 是 refuseIfAllMemberGroup 的按群号版本，供还没有
// 群行在手的调用点使用。
//
// 读群行失败同样放行，理由同上。
func (g *Group) refuseIfAllMemberGroupByNo(c *wkhttp.Context, groupNo, action string) bool {
	if groupNo == "" {
		return false
	}
	groupModel, err := g.db.QueryWithGroupNo(groupNo)
	if err != nil {
		g.Error("查询群资料失败，放行全员群守卫",
			zap.Error(err), zap.String("groupNo", groupNo), zap.String("action", action))
		return false
	}
	return g.refuseIfAllMemberGroup(c, groupModel, action)
}

// respondAllMemberGroupProtected 写出 D7 的拒绝。
//
// details.action 告诉客户端被拒的是哪一个动作，它才能渲染出对应的引导文案
// （"请退出项目"而不是一句泛泛的"不允许"）。这不泄露任何东西：动作是调用方自己
// 发起的，而它已经是这个群的成员。
func respondAllMemberGroupProtected(c *wkhttp.Context, action string) {
	httperr.ResponseErrorL(c, errcode.ErrGroupAllMemberGroupProtected, nil, i18n.Details{
		"action": action,
	})
}
