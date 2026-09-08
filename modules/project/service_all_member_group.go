package project

import (
	"time"

	"go.uber.org/zap"
)

// 全员群的服务层：建、补建、同步（D4 / D6 / D8 / D12）。
//
// 本文件里的每一个函数都在**项目事务提交之后**被调用，都是 best-effort，都不会让
// 调用方的业务操作失败。理由见 all_member_group_registry.go 顶部；兜底见每个函数
// 自己的注释。

// provisionAllMemberGroup 尝试为一个项目建出全员群，并把 group_no 写回项目行。
//
// 幂等，且可以从任意多个写路径并发调用——互斥由 claimAllMemberGroupProvision 的
// CAS 租约承担，不是由调用方的编排承担。这一点是有意的：补建的触发点是"任何一次
// 写路径发现全员群还不存在"，而那些写路径彼此之间没有任何协调。
//
// 全部失败模式都收敛到同一个结果：项目还活着，all_member_group_no 还是空串，
// 下一个写路径会再试一次，I4 扫描 A 把它报出来。没有任何一条会让调用方失败。
func (p *Project) provisionAllMemberGroup(projectID, spaceID, creator, name string, members []string) {
	provision := allMemberGroupProvisioner()
	if provision == nil {
		// 只包含 modules/project 而不含 modules/group 的二进制会走到这里。
		// 不回退项目、不 panic：项目本身是完整的，缺的是它的群。
		p.Error("全员群创建器未注册，跳过建群（项目已创建，可由后续写路径补建）",
			zap.String("projectId", projectID), zap.String("spaceId", spaceID))
		observeAllMemberGroupProvisionFailure(reasonProvisionerMissing)
		return
	}

	claimed, lease, err := p.db.claimAllMemberGroupProvision(projectID, time.Now().UTC())
	if err != nil {
		p.Error("认领全员群建群失败", zap.Error(err), zap.String("projectId", projectID))
		observeAllMemberGroupProvisionFailure(reasonProvisionClaimFailed)
		return
	}
	if !claimed {
		// 三种情况合成一个：已经有群了、别人正在建、项目已解散。
		// 对本函数都是"没有活可干"，不必区分。
		return
	}

	groupNo, err := provision(p.ctx, AllMemberGroupSeed{
		ProjectID: projectID,
		SpaceID:   spaceID,
		Creator:   creator,
		Name:      name,
		Members:   members,
	})
	if err != nil || groupNo == "" {
		p.Error("创建全员群失败（项目已创建，all_member_group_no 留空，由后续写路径补建）",
			zap.Error(err), zap.String("projectId", projectID), zap.String("spaceId", spaceID))
		observeAllMemberGroupProvisionFailure(reasonProvisionCallFailed)
		// 主动放弃租约，让下一次写路径立刻能重试，而不是干等满一个租约周期。
		// 围栏在本次认领的 deadline 上：一次超时的尝试不得清掉后继者的租约。
		if relErr := p.db.releaseAllMemberGroupProvision(projectID, lease); relErr != nil {
			p.Warn("释放全员群建群租约失败（租约到期后仍会自动释放）",
				zap.Error(relErr), zap.String("projectId", projectID))
		}
		return
	}

	ok, err := p.db.setAllMemberGroupNo(projectID, groupNo)
	if err != nil {
		p.Error("写回全员群失败（群已建出，项目行未更新，由 I4 扫描 A 报出）",
			zap.Error(err), zap.String("projectId", projectID), zap.String("groupNo", groupNo))
		observeAllMemberGroupProvisionFailure(reasonProvisionWriteBackFailed)
		return
	}
	if !ok {
		// 租约在建群期间过期、别人接手并已经建成。刚建出来的这个群不是全员群，
		// 它是一个普通项目群（group.project_id 已指向本项目，I2 照常约束它）。
		// 记下来让它可查，由 I4 扫描 A 报出——不静默删除：删一个已经建好、已经
		// 发出创建通知、已经有 IM 频道的群，比留着它更危险。
		p.Warn("全员群写回落空：租约期间已有另一个群被登记为全员群，本次建出的群将作为普通项目群留存",
			zap.String("projectId", projectID), zap.String("groupNo", groupNo))
		observeAllMemberGroupProvisionFailure(reasonProvisionRaceLost)
		return
	}
	p.Info("全员群已创建",
		zap.String("projectId", projectID), zap.String("groupNo", groupNo),
		zap.Int("initialMembers", len(members)+1))
}

// ensureAllMemberGroup 是补建入口：只在项目还没有全员群时才动手（D4 的 (c)）。
//
// 挂在写路径上而不是对账上，这是有意的取舍。对账扫描在本仓库是**只报不修**的
// （P0/P1 的五个扫描无一例外），而补建要写群表——让对账 worker 持有一条写路径，
// 就等于让"监控"和"修复"共用一个进程，一个 bug 能同时毁掉两者。
//
// 挂在写路径上还有一个好处：补建发生在用户正在操作这个项目的时刻，失败对他是可见
// 的（下一次加人还会再试），而不是在一个没人看的后台里反复失败。
// 返回补建之后这个项目的全员群号（""=仍然没有）。调用方据此把群号带进批量入群，
// 而不是每个 uid 再查一次——见 admitAllMemberGroupTo。
func (p *Project) ensureAllMemberGroup(projectID, spaceID string) string {
	groupNo, err := p.db.queryAllMemberGroupNo(projectID)
	if err != nil {
		p.Warn("查询全员群失败，跳过补建", zap.Error(err), zap.String("projectId", projectID))
		// 这一次查询同时服务两件事：决定要不要补建，以及给随后的批量入群提供群号。
		// 它失败时整批入群都会静默空转，所以入群失败的指标要在这里记——否则把
		// 查询提到循环外面这个优化就顺手吃掉了一个信号。
		observeAllMemberGroupAdmitFailure(reasonAdmitLookupFailed)
		return ""
	}
	if groupNo != "" {
		return groupNo
	}
	// The lookup says "no group", but the POINTER may still be set — that is
	// exactly the detached/disbanded case, and the claim CAS below keys on the
	// pointer being empty. Without clearing it first the claim can never succeed:
	// the rebuild would silently never run, and every later member's admission
	// would no-op, with reconcile scan A reporting a project nothing repairs.
	//
	// Cleared here rather than inside the claim so the claim stays a single
	// unconditional CAS, and so this correction is visible in the log.
	cleared, err := p.db.clearStaleAllMemberGroupPointer(projectID)
	if err != nil {
		p.Warn("清理失效的全员群指针失败，跳过补建", zap.Error(err), zap.String("projectId", projectID))
		return ""
	}
	if cleared {
		p.Warn("全员群指针已失效（群被解散或已脱离本项目），已清空，准备补建",
			zap.String("projectId", projectID), zap.String("spaceId", spaceID))
	}
	row, err := p.db.queryByProjectID(projectID)
	if err != nil || row == nil || row.Status != StatusNormal {
		return ""
	}
	// 群主取**当前活跃的 owner**，不是 octo_project.creator。
	//
	// creator 记的是当初谁建的项目，永不改变；补建发生在多久之后是不确定的，
	// 那个人可能早已离开项目或 Space。拿他去建群会被准入闸门当场拒掉（他不是
	// 项目活跃成员），于是一个初次建群失败过的项目**永远**补建不出来——每次写
	// 路径都认领租约、建群失败、释放租约，无限循环，而 I4 扫描 A 会永远报着它。
	// 这条正是补建存在的意义所在，用错人等于补建从来没生效过。
	owner, err := p.db.queryActiveOwnerForProvision(projectID)
	if err != nil {
		p.Warn("查询项目 owner 失败，跳过补建", zap.Error(err), zap.String("projectId", projectID))
		return ""
	}
	if owner == "" {
		// 无主项目（P0 的 Space 级联可以造出这种状态并留了 Warn）。没有人能当群主，
		// 补建只会失败；等有 owner 了再说。I4 扫描 A 继续报着它。
		p.Warn("项目暂无活跃 owner，跳过全员群补建",
			zap.String("projectId", projectID), zap.String("spaceId", spaceID))
		return ""
	}
	// 补建的初始成员是**当前全体活跃成员**，不是只有 owner。
	//
	// 前一版传 nil，并写着"剩下的人由 admitAllMemberGroup 逐个补进去"。没有任何
	// 东西会那么做：admitAllMemberGroup 只对触发这次补建的那个请求里的 uid 调用。
	// 于是一个已经有 A、B 的项目在加 C 的时候补建，建出来的群是 {owner, C}——
	// A 和 B 谁也不会把他们放进去，扫描 B 报出缺口而按决策不自动修复，唯一的
	// 补救是管理员一个一个重新加。那句注释描述的是一个不存在的机制。
	//
	// 那一版给的理由是"批量入群是全或无的，一个人被 I2 拒绝就整次建群失败"。
	// 理由成立但结论反了：这里读的就是项目的活跃成员，按定义全部通过 I2；唯一
	// 会失败的情况是读名册和建群之间有人被移除，而那种失败是**自愈**的——租约
	// 释放，下一个写路径带着新名册重试。用一次偶发重试换掉一个静默不完整的群，
	// 这笔交易是划算的。
	//
	// 有界：名册不可能超过项目的 max_members（每条加人路径都在事务里数过），
	// 取满即数据异常，记一条 Warn。
	maxMembers := p.cfg.effectiveMaxMembers(row.MaxMembers)
	roster, err := p.db.queryActiveMemberUIDsForRebuild(projectID, maxMembers)
	if err != nil {
		p.Warn("读取项目名册失败，跳过全员群补建", zap.Error(err), zap.String("projectId", projectID))
		return ""
	}
	if len(roster) >= maxMembers {
		p.Warn("补建全员群：名册取满上限，可能有成员未被带入新群（由 I4 扫描 B 报出）",
			zap.String("projectId", projectID), zap.Int("limit", maxMembers))
	}
	// owner 由 CreateGroup 自己作为群主加入，出现在 Members 里会变成重复插入。
	p.provisionAllMemberGroup(projectID, spaceID, owner, row.Name, withoutUID(roster, owner))

	// 再读一次而不是让 provisionAllMemberGroup 返回群号：补建可能因为任何一种
	// 原因没成（钩子没注册、认领失败、建群失败、写回落空），而这些分支各自的
	// 处置不同，唯一统一的答案是"现在这个项目到底有没有全员群"。
	groupNo, err = p.db.queryAllMemberGroupNo(projectID)
	if err != nil {
		p.Warn("补建后复查全员群失败", zap.Error(err), zap.String("projectId", projectID))
		return ""
	}
	return groupNo
}

// admitAllMemberGroup 把一个 uid 放进项目的全员群（D12）。
//
// best-effort：失败只记日志加指标，不回滚项目席位。席位是授权事实，群是它的投影；
// 反过来会让"加人"因为一次 IM 抖动而失败，而那次失败对用户是不可解释的——他要做的
// 事（给项目加一个人）明明已经做成了。
//
// 漏网的人由 I4 扫描 B 报出，且**不自动修复**：修复要写群表，理由同 ensureAllMemberGroup。
// 群号由调用方传入，不在这里查。前一版每个 uid 都重跑一次 queryAllMemberGroupNo
// ——那是一条 octo_project ⨝ group 的连接查询——所以一次 200 人的加人会发 200 次
// 同样的查询，而 addMembers 在循环之前就已经调过 ensureAllMemberGroup 拿到了答案。
//
// 群号在批次中途变化（被 detach、被解散）不需要靠逐个重查来防：群侧的准入器自己
// 会重读群行并拒绝已解散的群，而重查读到的同样只是一个更晚一点的快照，挡不住
// 同一件事。
func (p *Project) admitAllMemberGroup(projectID, spaceID, groupNo, uid string) {
	if uid == "" {
		return
	}
	admit := allMemberGroupAdmitter()
	if admit == nil {
		observeAllMemberGroupAdmitFailure(reasonAdmitterMissing)
		return
	}
	if groupNo == "" {
		// 还没有全员群。这不是入群失败，是补建的活；调用方已经在这之前调过
		// ensureAllMemberGroup，所以走到这里说明补建也没成功——那一路已经记过
		// 日志和指标了，这里不重复计数。
		return
	}
	if err := admit(p.ctx, spaceID, groupNo, uid); err != nil {
		p.Error("加入全员群失败（项目席位已生效，人未进群，由 I4 扫描 B 报出）",
			zap.Error(err), zap.String("projectId", projectID),
			zap.String("groupNo", groupNo), zap.String("uid", uid))
		observeAllMemberGroupAdmitFailure(reasonAdmitCallFailed)
	}
}

// syncAllMemberGroupOwner 确保全员群的群主仍是本项目的活跃 owner（D6）。
//
// 无条件调用即可——判定在群侧，是幂等的（见 AllMemberGroupOwnerTransfer 的注释：
// 项目可以有多个 owner，所以"换成刚被提升的那个人"是错的）。
//
// 不同步会留下一个谁也动不了的群主：D7 之后，一个普通项目成员当着全员群的群主，
// 既不能解散也不能退群也不能转让。兜底是 P1 的级联——原 owner 一旦离开项目，
// detach 会按"群主离开"路径把群主移交给资深的项目成员。
func (p *Project) syncAllMemberGroupOwner(projectID string) {
	transfer := allMemberGroupOwnerTransfer()
	if transfer == nil {
		return
	}
	groupNo, err := p.db.queryAllMemberGroupNo(projectID)
	if err != nil || groupNo == "" {
		return
	}
	if err := transfer(p.ctx, projectID, groupNo); err != nil {
		p.Error("同步全员群群主失败（项目 owner 已变动，群主未跟上）",
			zap.Error(err), zap.String("projectId", projectID),
			zap.String("groupNo", groupNo))
		observeAllMemberGroupSyncFailure(reasonSyncOwner)
	}
}

// syncAllMemberGroupName 把全员群名改成项目名（D8）。best-effort。
//
// 群名上限（50 rune）短于项目名上限（64 rune），截断由群侧做——截断规则属于群。
func (p *Project) syncAllMemberGroupName(projectID, name string) {
	if name == "" {
		return
	}
	rename := allMemberGroupRename()
	if rename == nil {
		return
	}
	groupNo, err := p.db.queryAllMemberGroupNo(projectID)
	if err != nil || groupNo == "" {
		return
	}
	if err := rename(p.ctx, groupNo, name); err != nil {
		p.Error("同步全员群名失败（项目已改名，群名未变）",
			zap.Error(err), zap.String("projectId", projectID),
			zap.String("groupNo", groupNo))
		observeAllMemberGroupSyncFailure(reasonSyncName)
	}
}
