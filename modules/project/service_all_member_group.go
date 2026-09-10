package project

import (
	"time"

	"go.uber.org/zap"
)

// 全员群的服务层：建、改名（D4 / D8）。
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
		// 本项目，关联元数据与原生群成员彼此独立）。不静默删除：删一个已经建好、已经发出创建通知、
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

// syncAllMemberGroupName 把全员群名改成项目名（D8）。best-effort。
//
// 群名上限（50 rune）短于项目提交名上限（30 rune），但历史项目名仍可能达到 legacy 64
// rune；截断由群侧做——截断规则属于群。
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
	if err := rename(p.ctx, projectID, groupNo, name); err != nil {
		p.Error("同步全员群名失败（项目已改名，群名未变）",
			zap.Error(err), zap.String("projectId", projectID),
			zap.String("groupNo", groupNo))
		observeAllMemberGroupSyncFailure(reasonSyncName)
	}
}
