package project

import (
	"errors"
	"time"

	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
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

	// 围栏在本次认领的 deadline 上，与 releaseAllMemberGroupProvision 同一条理由的
	// 另一半：一次超时的尝试既不得清掉后继者的租约，也不得用它的旧名册快照顶掉
	// 后继者建好的群。
	ok, err := p.db.setAllMemberGroupNo(projectID, groupNo, lease)
	if err != nil {
		p.Error("写回全员群失败（群已建出，项目行未更新，由 I4 扫描 A 报出）",
			zap.Error(err), zap.String("projectId", projectID), zap.String("groupNo", groupNo))
		observeAllMemberGroupProvisionFailure(reasonProvisionWriteBackFailed)
		return
	}
	if !ok {
		// 写回落空，三种情况，处置相同而**说法必须准确**：
		//
		//  1. 别人接手并已经登记了另一个群（指针非空）；
		//  2. 别人只是**认领**了（租约被换成他的 deadline），指针还是空的，此刻
		//     还没有任何群被登记 —— 围栏加上之后新增的这一种；
		//  3. 项目在建群期间被解散（status 已不是 Normal），因此不在扫描 A 的
		//     定义域里，报出它的是 I3 而不是 A。
		//
		// 上一版的文案只写了第 1 种并断言"由 I4 扫描 A 报出"，于是在另外两种情况下
		// 把 on-call 指到错的地方。PR #855 第五轮 review。
		//
		// 无论哪一种，刚建出来的这个群都是一个普通项目群（group.project_id 已指向
		// 本项目，I2 照常约束它）。不静默删除：删一个已经建好、已经发出创建通知、
		// 已经有 IM 频道的群，比留着它更危险。
		p.Warn("全员群写回落空（指针已被他人登记 / 租约已被他人认领 / 项目已解散），本次建出的群将作为普通项目群留存",
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
	// maxMembers + 1：多读一行，才分得清"正好满员"和"被截断"。
	//
	// 上一轮这里传的是 maxMembers，于是 len(roster) > maxMembers 按构造不可能成立，
	// 那条本该让截断可见的 Warn 是死代码——而同一个 commit 里还写着「名册**不**截断，
	// 这是一个决定」。代码在截断，只是不说。
	//
	// 截断是可达的：updateProject 接受 max_members 且不校验当前活跃席位数，所以把一个
	// 已有 300 人的项目改成 200，之后每一次补建都只会带进 200 个人。
	maxMembers := p.cfg.effectiveMaxMembers(row.MaxMembers)

	// owner 候选按成员配额取，不是按一个自选的小常数：owner 是成员的子集，所以配额
	// 就是这里天然的界。写死一个小数会让"最资深的 N 位恰好都是 I1 泄漏"变成一次
	// 永久失败——正是下面那段要修的缺陷的窄版本。
	ownerCandidates, err := p.db.queryActiveOwnerCandidatesForProvision(projectID, maxMembers)
	if err != nil {
		p.Warn("查询项目 owner 失败，跳过补建", zap.Error(err), zap.String("projectId", projectID))
		return ""
	}
	if len(ownerCandidates) == 0 {
		// 无主项目（P0 的 Space 级联可以造出这种状态并留了 Warn）。没有人能当群主，
		// 补建只会失败；等有 owner 了再说。I4 扫描 A 继续报着它。
		p.Warn("项目暂无活跃 owner，跳过全员群补建",
			zap.String("projectId", projectID), zap.String("spaceId", spaceID))
		return ""
	}

	roster, err := p.db.queryActiveMemberUIDsForRebuild(projectID, maxMembers+1)
	if err != nil {
		p.Warn("读取项目名册失败，跳过全员群补建", zap.Error(err), zap.String("projectId", projectID))
		return ""
	}
	if len(roster) > maxMembers {
		// 多读的那一行只是探针，不进群。
		roster = roster[:maxMembers]
		// 计数而不只是记日志：日志里的信号断言不了，于是它被静默删掉过一次
		// （上一轮把 >= 改成 > 却没改 LIMIT），而变异测试证明它还能再被删一次。
		// 见 allMemberGroupRosterTruncated 的注释。
		observeAllMemberGroupRosterTruncated(reasonTruncatedOverMaxMembers)
		p.Warn("补建全员群：项目活跃成员多于 max_members，名册被截断，超出的成员未带入新群（由 I4 扫描 B 报出）",
			zap.String("projectId", projectID), zap.Int("maxMembers", maxMembers))
	}

	// 名册和 owner 候选都要先过 Space 席位这一关，因为**建群会拿它们去过**。
	//
	// 补建的两个输入都读自 octo_project_member，而 CreateGroup 用 space_member 校验：
	// 群主走 CheckMembership（不过就整个失败），成员走 I2 准入闸门的 Space 半边
	// （ActiveMembers，一个不过就整批拒）。项目席位**不蕴含** Space 席位——这正是
	// 那个闸门是合取的原因，也是 i1_violations 和 i1_abandoned_cleanup_leak 是两个
	// 独立指标的原因。
	//
	// 上一版这里写着「读的就是项目的活跃成员，按定义全部通过 I2」。那句话漏了 Space
	// 那一半，而漏掉的正好是可达且**终局**的一半：Space 移除工单耗尽重试后放弃，会
	// 留下一个项目席位还活着、Space 席位已经没了的 uid，没有任何东西会再来处理它。
	// 于是一个这样的成员就让补建整个失败，而且因为输入是确定性全序，每一次加人都
	// 以完全相同的方式失败——补建是这个功能唯一的修复路径，扫描 A 只报不修。
	//
	// 一次批量查询而不是逐个查：ActiveMembers 一条语句读完，与准入闸门的 Space 半边
	// 用的是同一个函数，所以两边不会对"谁是活跃 Space 成员"给出不同答案。
	//
	// 被筛掉的人按定义就是 I1 违规，扫描 B 会把他们报成缺口——那是它们该待的地方，
	// 而且比"整个项目没有群"好得多。
	probe := append(append([]string{}, ownerCandidates...), roster...)
	spaceActive, err := spacepkg.ActiveMembers(p.db.session, spaceID, probe)
	if err != nil {
		p.Warn("校验补建成员的 Space 席位失败，跳过补建",
			zap.Error(err), zap.String("projectId", projectID))
		return ""
	}
	owner := ""
	for _, candidate := range ownerCandidates {
		if spaceActive[candidate] {
			owner = candidate
			break
		}
	}
	if owner == "" {
		p.Warn("项目的活跃 owner 都没有 Space 席位，跳过全员群补建（I1 泄漏，需人工处理）",
			zap.String("projectId", projectID), zap.String("spaceId", spaceID),
			zap.Int("candidates", len(ownerCandidates)))
		return ""
	}
	admissible := make([]string, 0, len(roster))
	dropped := make([]string, 0)
	for _, uid := range roster {
		if uid == owner {
			// owner 由 CreateGroup 自己作为群主加入，出现在 Members 里会变成重复插入。
			continue
		}
		if spacepkg.IsSystemBot(uid) || spaceActive[uid] {
			admissible = append(admissible, uid)
			continue
		}
		dropped = append(dropped, uid)
	}
	if len(dropped) > 0 {
		p.Warn("补建全员群：部分项目成员没有 Space 席位，未带入新群（I1 泄漏，由 I4 扫描 B 报出）",
			zap.String("projectId", projectID), zap.String("spaceId", spaceID),
			zap.Strings("dropped", dropped))
	}

	// # 名册**不**截断，这是一个决定
	//
	// PR #855 第二轮 review 的 Q1：补建的代价是 O(名册)，而且发生在一次加人请求的
	// 同步路径上。可选项是按 MemberBatchMax 截断、让扫描 B 兜住尾巴。不截，理由是
	// 两条：
	//
	//  1. 截断把"补建"变回上一轮刚修掉的那个缺陷——建出一个**不完整而且没人会补全**
	//     的群。尾巴的唯一补救仍然是管理员逐个重加，那正是 Q11 判定为不可接受的。
	//  2. 这个代价是**一次性**的，不是每请求的。上面那条 P1 修复拿掉了让补建永久
	//     失败的那个状态，所以补建成功之后就不再发生；而它失败时，租约会释放、下一次
	//     写路径重试——重试的次数取决于失败原因，不取决于名册大小。
	//
	// 顺序往返那一半单独解决了：CreateGroup 的逐个 CheckMembership 循环已经换成
	// 一条 ActiveMembers 批量查询。剩下的是每个成员一次 GenSeq，那是建群路径对所有
	// 调用方都有的形状，改它要动版本号的分配方式，不搭这一版的车。
	if len(admissible)+1 > p.cfg.MemberBatchMax {
		p.Info("补建全员群：初始成员数超过单批加人上限，属预期（补建是一次性的，见此处注释）",
			zap.String("projectId", projectID), zap.Int("members", len(admissible)+1),
			zap.Int("batchMax", p.cfg.MemberBatchMax))
	}
	p.provisionAllMemberGroup(projectID, spaceID, owner, row.Name, admissible)

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
		// 还没有全员群。这不是入群失败，是补建的活；调用方在这之前调过
		// ensureAllMemberGroup，所以走到这里说明那一路没能给出群号。
		//
		// 两种情况，而且必须分开记：补建**失败**了（那一路已经记过日志和指标，
		// 这里重复计数只会让同一件事在两个指标上各响一次），或者补建被**跳过**了
		// ——另一个写路径正握着租约，认领 CAS 影响 0 行就直接返回。后者原本什么
		// 都不记，理由写的是"补建那一路已经记过"，而那条理由恰恰在这里不成立：
		// 没跑，就没记。于是一次并发的加人会把整批人的入群静默丢掉，直到扫描 B
		// 过了宽限期才看得见。
		//
		// 无从在这里分辨是哪一种，所以按"输了竞态"记：失败那一路自己已经有指标，
		// 这个计数器的价值在于让"整批入群什么都没发生"这件事在仪表盘上有痕迹。
		// 只记指标不记日志：这个分支是逐 uid 的，一次 200 人的加人会写 200 行同样的
		// 话。批次级的那一条由 addMembers 记（它知道整批的规模）。
		observeAllMemberGroupAdmitFailure(reasonAdmitSkippedNoGroup)
		return
	}
	if err := admit(p.ctx, spaceID, groupNo, uid); err != nil {
		// 两种失败，两种残留，两条不同的处置——上一版把它们合成了一条，而那条话
		// 对其中一种是错的。PR #855 第七轮 review 的 P2-3。
		if errors.Is(err, projectpkg.ErrAdmittedButNotSubscribed) {
			p.Error("全员群订阅失败（人已在群里，但 broker 侧没有订阅：收不到消息、"+
				"子区里发不了言。I4 扫描 B 看不见这种状态——它比对的是 group_member 行）",
				zap.Error(err), zap.String("projectId", projectID),
				zap.String("groupNo", groupNo), zap.String("uid", uid))
			observeAllMemberGroupAdmitFailure(reasonAdmitSubscribeFailed)
			return
		}
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
