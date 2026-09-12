package project

import (
	"errors"
	"strings"
	"time"

	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

// 全员群的服务层：建、准入、群主同步、改名（D4 / D8）。
//
// 本文件里的每一个函数都在**项目事务提交之后**被调用，都是 best-effort，都不会让
// 调用方的业务操作失败。理由见 all_member_group_registry.go 顶部；兜底见每个函数
// 自己的注释。

// provisionAllMemberGroup attempts to create the all-member group for a project
// and persist its group_no. The runtime trigger is the post-commit project
// creation hook; the lease still makes any repeated invocation idempotent.
//
// Every failure leaves the Project alive with all_member_group_no empty. Scan A
// reports that missing artifact for explicit operational repair; this best-effort
// hook never turns a successful Project write into an error.
func (p *Project) provisionAllMemberGroup(projectID, spaceID, creator, name string, members []string) {
	provision := allMemberGroupProvisioner()
	if provision == nil {
		// A binary containing only modules/project has no group-side provisioner.
		// Do not roll back or panic: the Project is complete; scan A reports the
		// missing all-member group for explicit repair.
		p.Error("全员群创建器未注册，跳过建群（项目已创建，I4 扫描 A 将报告）",
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
		p.Error("创建全员群失败（项目已创建，I4 扫描 A 将报告）",
			zap.Error(err), zap.String("projectId", projectID), zap.String("spaceId", spaceID))
		observeAllMemberGroupProvisionFailure(reasonProvisionCallFailed)
		// Release the lease so an explicit repair can run without waiting for its
		// full duration. The fence still prevents this attempt from clearing a
		// successor's lease.
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

// ensureAllMemberGroup returns the active Project's dedicated group, repairing
// a missing or stale pointer through the same post-commit lease protocol used
// by initial creation. The rebuild seed is a bounded snapshot of the current
// active Project roster, filtered to people who still hold a Space seat so the
// native CreateGroup admission gate cannot reject the whole rebuild.
func (p *Project) ensureAllMemberGroup(projectID, spaceID string) string {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return ""
	}
	groupNo, err := p.db.queryAllMemberGroupNo(projectID)
	if err != nil {
		p.Warn("查询全员群失败，跳过补建",
			zap.String("projectId", projectID), zap.Error(err))
		return ""
	}
	if groupNo != "" {
		return groupNo
	}

	cleared, err := p.db.clearStaleAllMemberGroupPointer(projectID)
	if err != nil {
		p.Warn("清理失效的全员群指针失败，跳过补建",
			zap.String("projectId", projectID), zap.Error(err))
		return ""
	}
	if cleared {
		p.Warn("全员群指针已失效，已清空并准备补建",
			zap.String("projectId", projectID), zap.String("spaceId", spaceID))
	}

	model, err := p.db.queryByProjectID(projectID)
	if err != nil || model == nil || model.Status != StatusNormal {
		return ""
	}
	spaceID = strings.TrimSpace(spaceID)
	if spaceID == "" {
		spaceID = model.SpaceID
	}
	if spaceID == "" {
		return ""
	}
	maxMembers := p.cfg.effectiveMaxMembers(model.MaxMembers)
	if maxMembers <= 0 {
		return ""
	}

	ownerCandidates, err := p.db.queryActiveOwnerCandidatesForProvision(projectID, maxMembers)
	if err != nil {
		p.Warn("查询项目 owner 失败，跳过补建",
			zap.String("projectId", projectID), zap.Error(err))
		return ""
	}
	roster, err := p.db.queryActiveMemberUIDsForRebuild(projectID, maxMembers+1)
	if err != nil {
		p.Warn("读取项目名册失败，跳过补建",
			zap.String("projectId", projectID), zap.Error(err))
		return ""
	}
	if len(roster) > maxMembers {
		roster = roster[:maxMembers]
		p.Warn("补建全员群：项目活跃成员超过 max_members，名册被截断",
			zap.String("projectId", projectID), zap.Int("maxMembers", maxMembers))
	}
	probe := make([]string, 0, len(ownerCandidates)+len(roster))
	probe = append(probe, ownerCandidates...)
	probe = append(probe, roster...)
	spaceActive, err := spacepkg.ActiveMembers(p.db.session, spaceID, probe)
	if err != nil {
		p.Warn("校验补建成员的 Space 席位失败，跳过补建",
			zap.String("projectId", projectID), zap.Error(err))
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
		// An active Project without a usable human Space owner is a valid
		// intermediate state during Space cleanup. Do not create a group whose
		// creator cannot pass native group admission.
		return ""
	}
	members := make([]string, 0, len(roster))
	for _, uid := range roster {
		if uid == owner || spaceActive[uid] || spacepkg.IsSystemBot(uid) {
			if uid != owner {
				members = append(members, uid)
			}
		}
	}
	p.provisionAllMemberGroup(projectID, spaceID, owner, model.Name, members)
	groupNo, err = p.db.queryAllMemberGroupNo(projectID)
	if err != nil {
		p.Warn("补建全员群后读取指针失败",
			zap.String("projectId", projectID), zap.Error(err))
		return ""
	}
	return groupNo
}

// admitAllMemberGroup performs one idempotent, pointer-scoped admission. The
// Project transaction has already committed, so failures are best-effort and
// are retried by a later Project write or explicit reconciliation.
func (p *Project) admitAllMemberGroup(spaceID, groupNo, uid string) {
	admit := allMemberGroupAdmitter()
	if admit == nil || strings.TrimSpace(spaceID) == "" ||
		strings.TrimSpace(groupNo) == "" || strings.TrimSpace(uid) == "" {
		return
	}
	if err := admit(p.ctx, spaceID, groupNo, uid); err != nil {
		if errors.Is(err, projectpkg.ErrAdmittedButNotSubscribed) {
			p.Error("全员群订阅失败（成员已入群，后续写路径会重试）",
				zap.String("projectId", ""), zap.String("groupNo", groupNo),
				zap.String("uid", uid), zap.Error(err))
			return
		}
		p.Warn("同步成员到全员群失败（后续加回/修复会重试）",
			zap.String("groupNo", groupNo),
			zap.String("uid", uid),
			zap.Error(err))
	}
}

// syncAllMemberGroupOwner converges the dedicated native group owner to the
// active human Project owner. It is intentionally pointer-only and retryable.
func (p *Project) syncAllMemberGroupOwner(projectID string) {
	transfer := allMemberGroupOwnerTransfer()
	if transfer == nil || strings.TrimSpace(projectID) == "" {
		return
	}
	groupNo, err := p.db.queryAllMemberGroupNo(projectID)
	if err != nil || groupNo == "" {
		if err != nil {
			p.Warn("读取全员群指针以同步群主失败",
				zap.String("projectId", projectID), zap.Error(err))
		}
		return
	}
	if err := transfer(p.ctx, projectID, groupNo); err != nil {
		p.Warn("同步全员群群主失败（后续项目变更会重试）",
			zap.String("projectId", projectID),
			zap.String("groupNo", groupNo),
			zap.Error(err))
	}
}

// syncAllMemberGroupMembers ensures the dedicated group exists after a
// successful Project membership commit, then replays every requested uid.
func (p *Project) syncAllMemberGroupMembers(projectID, spaceID string, uids []string) {
	if len(uids) == 0 {
		return
	}
	groupNo := p.ensureAllMemberGroup(projectID, spaceID)
	if groupNo == "" {
		return
	}
	for _, uid := range uids {
		p.admitAllMemberGroup(spaceID, groupNo, uid)
	}
	p.syncAllMemberGroupOwner(projectID)
}
