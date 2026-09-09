package group

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
	"go.uber.org/zap"
)

// D7 —— 全员群受保护的**五个**操作。
//
// 全员群的成员集合**等于**项目的成员集合（不变量 I4）。所以群面上会改变成员集合
// 或群归属的操作，各自都有一个项目侧的等价物，必须走那边：
//
//	群面动作            → 项目侧等价物
//	退群 exit           → 退出项目
//	踢人 members 删除   → 从项目移除成员
//	拉黑 blacklist add  → 从项目移除成员
//	转让群主 transfer   → 转让项目 owner
//	解散群 disband      → 解散项目（没有别的等价物：群随项目结束而结束）
//
// 拉黑是 D7 原文没列的第五条，也是最容易漏的一条——它不叫"移除"、不走
// RemoveGroupMembers，只把 group_member.status 翻掉再退订，而对 I4 的效果与踢人
// 完全相同。解除拉黑不挡：那是把人放回活跃集合，方向与 I4 一致。
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
// 把保护放进服务层会把这四类调用方全部挡掉——也就是说，为了维护 I4 而加的守卫，会先把
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
	// allMemberGroupActionBlacklist 是第五条会改变活跃成员集合的群面路径，而且是
	// 最容易被漏掉的一条：它不叫"移除"、不走 RemoveGroupMembers，只把
	// group_member.status 翻成 Blacklist 并做 IM 退订。对不变量 I4 而言效果与
	// 踢人相同。P1 把它列为准入路径 A11 时用过同样的论证——「它就是那件事，
	// 所以按那件事对待」。
	allMemberGroupActionBlacklist = "blacklist"
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
		// 计数，不只是记日志：fail-open 只有在**响亮**的时候才是对的取舍。注释里
		// 的论证假设这是一次抖动，而真正要命的形状（表结构变更、排序规则漂移）
		// 每一次调用都会失败，等于把 D7 整个关掉，而关掉这件事在仪表盘上必须看
		// 得见。见 AllMemberGroupGuardFailures。
		projectpkg.AllMemberGroupGuardFailures.WithLabelValues(action).Inc()
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

// 按群号的变体曾经存在，现在没有了，而这是 C1 纪律的结果而不是清理顺手删的。
//
// 它自己发一次 QueryWithGroupNo。transferGrouper 是唯一的调用点，而它下一行就是
// getGroupInfo —— 同一条查询。于是 Space 直属群多 1 次、普通项目群多 2 次，额度
// 分别是 0 和 1。四个调用点里三个传现成的群行，第四个只是因为"有个按群号的重载
// 用起来方便"就没传。
//
// 不留这个变体，是因为守卫比重载更强：现在**没有**一个能自己发查询的入口，C1 就
// 不是靠每个调用点记得遵守，而是没得违反。将来真有只拿到群号的调用点，把它加回来
// 的同时要说清楚为什么那里读不到群行——那才是它该有的代价。
// TestAllMemberGroupGuardTakesTheGroupRowFromItsCaller 钉住这一条。

// respondAllMemberGroupProtected 写出 D7 的拒绝。
//
// details.action 告诉客户端被拒的是哪一个动作，它才能渲染出对应的引导文案
// （"请退出项目"而不是一句泛泛的"不允许"）。
//
// # 泄露面：不是零，是有界
//
// 前一版这里写的是"这不泄露任何东西：动作是调用方自己发起的，而它已经是这个群的
// 成员"。后半句在五个调用点里有两个不成立：踢人和退群的守卫都排在"读调用方的群成员
// 身份"之前（api.go 的 memberRemove / groupExit），退群那一处还是被 IMRemoveSubscriber
// 的位置逼出来的——守卫必须在那次退订之前。
//
// 所以口径改成实话：一个**非成员**能从这条拒绝里读出"这个群是某个项目的全员群"。
// 这个泄露是有界的，因为同一个 handler 上游的 getGroupInfo 已经用 404 与否回答了
// "这个群存不存在"，多出来的只是"它属于某个项目"。把它写清楚，是因为将来有人要动
// 守卫的位置时，会引用这段话——引用一句错的，就会把它挪到 IMRemoveSubscriber 后面。
func respondAllMemberGroupProtected(c *wkhttp.Context, action string) {
	httperr.ResponseErrorL(c, errcode.ErrGroupAllMemberGroupProtected, nil, i18n.Details{
		"action": action,
	})
}
