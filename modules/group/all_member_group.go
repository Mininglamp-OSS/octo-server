package group

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	projectmod "github.com/Mininglamp-OSS/octo-server/modules/project"
	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

// 全员群的群面实现（P2）。
//
// The dedicated all-member group is a live projection of one Project. Ordinary
// Project-associated groups remain native snapshots and never enter these hooks.

// registerAllMemberGroupHooks 由 1module.go 在模块构造时调用，与
// registerProjectCascadeSteps 并列。
func (g *Group) registerAllMemberGroupHooks() {
	projectmod.RegisterAllMemberGroupProvisioner(g.provisionAllMemberGroup)
	projectmod.RegisterAllMemberGroupAdmitter(g.admitToAllMemberGroup)
	projectmod.RegisterAllMemberGroupOwnerTransfer(g.ensureAllMemberGroupOwner)
	projectmod.RegisterAllMemberGroupRename(g.renameAllMemberGroup)
}

type allMemberGroupProjectBinding struct {
	ProjectID string
	SpaceID   string
	GroupNo   string
	Creator   string
}

// projectBindingMatchFlagsTx asks MySQL to compare every cross-table
// identifier against the authoritative Project columns in one query. Go byte
// equality would reject historical rows that differ only by case or PAD
// SPACE, while separate probes would reread the same locked Project.
func projectBindingMatchFlagsTx(
	tx *dbr.Tx, projectID, groupNo, groupProjectID, groupSpaceID string,
) (projectBindingMatchFlags, error) {
	if tx == nil || projectID == "" {
		return projectBindingMatchFlags{}, nil
	}
	var rows []projectBindingMatchFlags
	if _, err := tx.SelectBySql(
		"SELECT "+
			"(project_id = ?) AS project_id_match, "+
			"(all_member_group_no = ?) AS pointer_match, "+
			"(space_id = ?) AS space_id_match "+
			"FROM `octo_project` WHERE project_id = ? LIMIT 1",
		groupProjectID, groupNo, groupSpaceID, projectID,
	).Load(&rows); err != nil {
		return projectBindingMatchFlags{}, fmt.Errorf("group: compare Project binding identities: %w", err)
	}
	if len(rows) == 0 {
		return projectBindingMatchFlags{}, nil
	}
	return rows[0], nil
}

type projectBindingMatchFlags struct {
	ProjectIDMatch int `db:"project_id_match"`
	PointerMatch   int `db:"pointer_match"`
	SpaceIDMatch   int `db:"space_id_match"`
}

// lockAllMemberGroupBindingTx takes the Project lock before the native group
// lock. Every dedicated-group mutation uses this order; ordinary group
// admission remains on its native path and is intentionally not changed.
func lockAllMemberGroupBindingTx(tx *dbr.Tx, groupNo string) (*allMemberGroupProjectBinding, error) {
	var projects []allMemberGroupProjectBinding
	if _, err := tx.SelectBySql(
		"SELECT project_id, space_id, all_member_group_no AS group_no "+
			"FROM `octo_project` WHERE all_member_group_no = ? AND status = 1 "+
			"LIMIT 1 FOR UPDATE",
		groupNo,
	).Load(&projects); err != nil {
		return nil, fmt.Errorf("group: lock all-member project: %w", err)
	}
	if len(projects) == 0 || projects[0].ProjectID == "" {
		return nil, nil
	}
	var groups []struct {
		ProjectID string `db:"project_id"`
		SpaceID   string `db:"space_id"`
		Creator   string `db:"creator"`
		Status    int    `db:"status"`
	}
	if _, err := tx.SelectBySql(
		"SELECT project_id, space_id, creator, status FROM `group` "+
			"WHERE group_no = ? LIMIT 1 FOR UPDATE",
		groupNo,
	).Load(&groups); err != nil {
		return nil, fmt.Errorf("group: lock all-member native group: %w", err)
	}
	if len(groups) == 0 || groups[0].Status == GroupStatusDisband {
		return nil, nil
	}
	flags, err := projectBindingMatchFlagsTx(
		tx, projects[0].ProjectID, groupNo, groups[0].ProjectID, groups[0].SpaceID,
	)
	if err != nil {
		return nil, err
	}
	if flags.ProjectIDMatch == 0 || flags.PointerMatch == 0 ||
		(projects[0].SpaceID != "" && flags.SpaceIDMatch == 0) {
		return nil, nil
	}
	return &allMemberGroupProjectBinding{
		ProjectID: projects[0].ProjectID,
		SpaceID:   projects[0].SpaceID,
		GroupNo:   groupNo,
		Creator:   groups[0].Creator,
	}, nil
}

// lockProjectGroupBindingTx locks a Project row before its candidate native
// group, then reports whether that group is the current dedicated projection.
// Space cleanup uses it even for ordinary Project-associated groups: locking
// the Project row prevents the pointer from changing between the decision and
// the native member delete.
func lockProjectGroupBindingTx(
	tx *dbr.Tx, projectID, groupNo string,
) (*allMemberGroupProjectBinding, bool, error) {
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(groupNo) == "" {
		return nil, false, nil
	}
	var projects []struct {
		ProjectID string `db:"project_id"`
		SpaceID   string `db:"space_id"`
		GroupNo   string `db:"group_no"`
		Status    int    `db:"status"`
	}
	if _, err := tx.SelectBySql(
		"SELECT project_id, space_id, all_member_group_no AS group_no, status "+
			"FROM `octo_project` WHERE project_id = ? LIMIT 1 FOR UPDATE",
		projectID,
	).Load(&projects); err != nil {
		return nil, false, fmt.Errorf("group: lock Project pointer for Space cleanup: %w", err)
	}
	var groups []struct {
		ProjectID string `db:"project_id"`
		SpaceID   string `db:"space_id"`
		Creator   string `db:"creator"`
		Status    int    `db:"status"`
	}
	if _, err := tx.SelectBySql(
		"SELECT project_id, space_id, creator, status FROM `group` "+
			"WHERE group_no = ? LIMIT 1 FOR UPDATE",
		groupNo,
	).Load(&groups); err != nil {
		return nil, false, fmt.Errorf("group: lock candidate group for Space cleanup: %w", err)
	}
	if len(projects) == 0 || len(groups) == 0 {
		return nil, false, nil
	}
	flags, err := projectBindingMatchFlagsTx(
		tx, projects[0].ProjectID, groupNo, groups[0].ProjectID, groups[0].SpaceID,
	)
	if err != nil {
		return nil, false, err
	}
	if projects[0].Status != projectmod.StatusNormal ||
		groups[0].Status == GroupStatusDisband ||
		flags.ProjectIDMatch == 0 || flags.PointerMatch == 0 ||
		(projects[0].SpaceID != "" && flags.SpaceIDMatch == 0) {
		return nil, false, nil
	}
	return &allMemberGroupProjectBinding{
		ProjectID: projects[0].ProjectID,
		SpaceID:   projects[0].SpaceID,
		GroupNo:   groupNo,
		Creator:   groups[0].Creator,
	}, true, nil
}

// admitToAllMemberGroup is the only Project-driven native admission path. It
// validates the dedicated pointer while holding the Project and group locks,
// then delegates the actual upsert to the native admission funnel.
func (g *Group) admitToAllMemberGroup(ctx *config.Context, spaceID, groupNo, uid string) error {
	if ctx == nil || strings.TrimSpace(spaceID) == "" ||
		strings.TrimSpace(groupNo) == "" || strings.TrimSpace(uid) == "" {
		return nil
	}
	version, err := ctx.GenSeq(common.GroupMemberSeqKey)
	if err != nil {
		return fmt.Errorf("group: generate all-member admission version: %w", err)
	}
	tx, err := ctx.DB().Begin()
	if err != nil {
		return fmt.Errorf("group: begin all-member admission: %w", err)
	}
	defer tx.RollbackUnlessCommitted()
	var spaceSeat []struct {
		SpaceID string `db:"space_id"`
	}
	if _, err := tx.SelectBySql(
		"SELECT s.space_id AS space_id FROM `space_member` sm "+
			"INNER JOIN `space` s ON s.space_id = sm.space_id AND s.status = 1 "+
			"WHERE sm.space_id = ? AND sm.uid = ? AND sm.status = 1 "+
			"LIMIT 1 FOR UPDATE",
		spaceID, uid,
	).Load(&spaceSeat); err != nil {
		return fmt.Errorf("group: check all-member Space seat: %w", err)
	}
	if len(spaceSeat) == 0 {
		return nil
	}
	canonicalSpaceID := spaceSeat[0].SpaceID

	binding, err := lockAllMemberGroupBindingTx(tx, groupNo)
	if err != nil {
		return err
	}
	if binding == nil {
		return nil
	}
	flags, err := projectBindingMatchFlagsTx(
		tx, binding.ProjectID, groupNo, binding.ProjectID, canonicalSpaceID,
	)
	if err != nil {
		return err
	}
	if flags.SpaceIDMatch == 0 {
		return nil
	}

	var active []int
	if _, err := tx.SelectBySql(
		"SELECT 1 FROM `octo_project_member` "+
			"WHERE project_id = ? AND space_id = ? AND uid = ? "+
			"AND status = 1 AND removing = 0 LIMIT 1",
		binding.ProjectID, canonicalSpaceID, uid,
	).Load(&active); err != nil {
		return fmt.Errorf("group: check all-member Project seat: %w", err)
	}
	if len(active) == 0 {
		return nil
	}
	var usable []int
	if _, err := tx.SelectBySql(
		"SELECT 1 FROM `user` WHERE uid = ? AND status = 1 "+
			"AND COALESCE(is_destroy, 0) <> 2 LIMIT 1 FOR SHARE",
		uid,
	).Load(&usable); err != nil {
		return fmt.Errorf("group: query all-member admission account: %w", err)
	}
	if len(usable) == 0 {
		return nil
	}
	robot := 0
	var robots []int
	if _, err := tx.SelectBySql(
		"SELECT COALESCE(robot, 0) FROM `user` WHERE uid = ? LIMIT 1",
		uid,
	).Load(&robots); err != nil {
		return fmt.Errorf("group: query all-member admission user: %w", err)
	}
	if len(robots) > 0 {
		robot = robots[0]
	}
	if err := g.db.admitOrRestoreMembersTx(tx, groupNo, []MemberAdmission{{
		UID:       uid,
		Version:   version,
		Role:      MemberRoleCommon,
		InviteUID: binding.Creator,
		Robot:     robot,
	}}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("group: commit all-member admission: %w", err)
	}
	if err := ctx.IMAddSubscriber(&config.SubscriberAddReq{
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Subscribers: []string{uid},
	}); err != nil {
		return fmt.Errorf("%w: group: subscribe all-member admission: %w",
			projectpkg.ErrAdmittedButNotSubscribed, err)
	}
	g.addUsersToGroupThreads(groupNo, []string{uid})
	return nil
}

type allMemberGroupNativeMember struct {
	UID        string `db:"uid"`
	Role       int    `db:"role"`
	Status     int    `db:"status"`
	IsExternal int    `db:"is_external"`
}

type allMemberGroupProjectOwner struct {
	UID       string    `db:"uid"`
	JoinedAt  time.Time `db:"joined_at"`
	CreatedAt time.Time `db:"created_at"`
}

// ensureAllMemberGroupOwner converges native creator roles to an active human
// Project owner. It always locks Project, then group, then group_member rows;
// ordinary groups never enter this path.
func (g *Group) ensureAllMemberGroupOwner(ctx *config.Context, projectID, groupNo string) error {
	if ctx == nil || strings.TrimSpace(projectID) == "" || strings.TrimSpace(groupNo) == "" {
		return nil
	}
	tx, err := ctx.DB().Begin()
	if err != nil {
		return fmt.Errorf("group: begin all-member owner sync: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	binding, err := lockAllMemberGroupBindingTx(tx, groupNo)
	if err != nil {
		return err
	}
	if binding == nil {
		return nil
	}
	flags, err := projectBindingMatchFlagsTx(
		tx, binding.ProjectID, "", projectID, "",
	)
	if err != nil {
		return err
	}
	if flags.ProjectIDMatch == 0 {
		return nil
	}

	var members []allMemberGroupNativeMember
	if _, err := tx.SelectBySql(
		"SELECT uid, role, status, is_external FROM group_member "+
			"WHERE group_no = ? AND is_deleted = 0 FOR UPDATE",
		groupNo,
	).Load(&members); err != nil {
		return fmt.Errorf("group: lock all-member native members: %w", err)
	}
	memberByUID := make(map[string]allMemberGroupNativeMember, len(members))
	creators := make([]string, 0, 1)
	for _, member := range members {
		memberByUID[member.UID] = member
		if member.Role == MemberRoleCreator &&
			member.Status == int(common.GroupMemberStatusNormal) {
			creators = append(creators, member.UID)
		}
	}

	var owners []allMemberGroupProjectOwner
	if _, err := tx.SelectBySql(
		"SELECT pm.uid, COALESCE(pm.joined_at, pm.created_at) AS joined_at, "+
			"pm.created_at FROM `octo_project_member` pm "+
			"INNER JOIN space_member sm ON sm.space_id = pm.space_id COLLATE utf8mb4_general_ci "+
			"AND sm.uid = pm.uid COLLATE utf8mb4_general_ci AND sm.status = 1 "+
			"INNER JOIN space s ON s.space_id = pm.space_id COLLATE utf8mb4_general_ci AND s.status = 1 "+
			"LEFT JOIN `user` u ON u.uid = pm.uid COLLATE utf8mb4_general_ci "+
			"WHERE pm.project_id = ? AND pm.space_id = ? AND pm.status = 1 "+
			"AND pm.removing = 0 AND pm.role = ? AND COALESCE(u.robot, 0) = 0 "+
			"AND COALESCE(u.status, 0) = 1 AND COALESCE(u.is_destroy, 0) <> 2 "+
			"ORDER BY joined_at ASC, pm.created_at ASC, pm.uid ASC",
		binding.ProjectID, binding.SpaceID, projectmod.RoleOwner,
	).Load(&owners); err != nil {
		return fmt.Errorf("group: query all-member Project owners: %w", err)
	}

	// An ownerless Project, or a Project owner whose dedicated-group admission
	// has not converged yet, is not a reason to promote a non-owner. Leave the
	// current creator in place and let the next membership hook retry.
	target := ""
	for _, owner := range owners {
		member, ok := memberByUID[owner.UID]
		if ok && member.Status == int(common.GroupMemberStatusNormal) &&
			member.IsExternal == 0 {
			target = owner.UID
			break
		}
	}
	if target == "" {
		return nil
	}
	var usableTarget []int
	if _, err := tx.SelectBySql(
		"SELECT 1 FROM `user` WHERE uid = ? AND status = 1 "+
			"AND COALESCE(is_destroy, 0) <> 2 LIMIT 1 FOR SHARE",
		target,
	).Load(&usableTarget); err != nil {
		return fmt.Errorf("group: query all-member owner account: %w", err)
	}
	if len(usableTarget) == 0 {
		return nil
	}

	changed := memberByUID[target].Role != MemberRoleCreator
	for _, uid := range creators {
		if uid != target {
			changed = true
			break
		}
	}
	if !changed {
		return nil
	}

	if memberByUID[target].Role != MemberRoleCreator {
		version, err := ctx.GenSeq(common.GroupMemberSeqKey)
		if err != nil {
			return fmt.Errorf("group: generate all-member owner version: %w", err)
		}
		if err := g.db.UpdateMemberRoleTx(
			groupNo, target, MemberRoleCreator, version, tx,
		); err != nil {
			return fmt.Errorf("group: promote all-member owner: %w", err)
		}
	}
	for _, uid := range creators {
		if uid == target {
			continue
		}
		version, err := ctx.GenSeq(common.GroupMemberSeqKey)
		if err != nil {
			return fmt.Errorf("group: generate former all-member owner version: %w", err)
		}
		if err := g.db.UpdateMemberRoleTx(
			groupNo, uid, MemberRoleCommon, version, tx,
		); err != nil {
			return fmt.Errorf("group: demote former all-member creator: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("group: commit all-member owner sync: %w", err)
	}
	// Role state is committed. CMD/channel delivery is best-effort: a retry
	// observing unchanged roles does not replay these notifications.
	if err := ctx.SendCMD(config.MsgCMDReq{
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		CMD:         common.CMDGroupMemberUpdate,
		Param:       map[string]interface{}{"group_no": groupNo},
	}); err != nil {
		return fmt.Errorf("group: notify all-member owner sync: %w", err)
	}
	ctx.SendChannelUpdateToGroup(groupNo)
	return nil
}

// provisionAllMemberGroup 为一个项目建出全员群，返回 group_no。
//
// # 走 CreateGroup，不自己拼 INSERT
//
// （于是关系字段与原生成员快照在同一建群事务内落地）、IM 频道创建与失败补偿、群创建通知、
//
// # 谁是初始成员
//
// CreateProjectGroup 在锁住 Project 和所有当前成员后，按该事务内的活跃成员
// 快照一次性写入原生群成员；seed.Members 只保留给调用方日志/兼容契约，不是
// 权限来源。之后 Project 成员变更不会同步原生群成员。
//
// 这条路径在项目事务提交后执行，因此失败时由项目侧补偿/重试机制兜底；群自身
// 的 IM 创建通知仍在 CreateProjectGroup 的业务事务提交后处理。
//
// # 项目功能开关
//
// 不在这里查。建项目本身已经被 requireWriteEnabled 挡过一道，开关关着的时候
// 根本产生不了需要建群的项目；而补建路径服务的是**已经存在**的项目，对它们
// 关掉开关的语义是"不再新建项目"，不是"不再修复已有项目的群"。
func (g *Group) provisionAllMemberGroup(ctx *config.Context, seed projectmod.AllMemberGroupSeed) (string, error) {
	if seed.ProjectID == "" || seed.SpaceID == "" || seed.Creator == "" {
		return "", errors.New("group: all-member group seed requires project, space and creator")
	}
	resp, err := g.groupService.CreateGroup(&CreateGroupServiceReq{
		Creator:   seed.Creator,
		Name:      seed.Name,
		SpaceID:   seed.SpaceID,
		ProjectID: seed.ProjectID,
	})
	if err != nil {
		return "", fmt.Errorf("group: create all-member group: %w", err)
	}
	if resp == nil || resp.GroupNo == "" {
		return "", errors.New("group: create all-member group returned no group_no")
	}
	g.Info("全员群已建出",
		zap.String("projectId", seed.ProjectID), zap.String("groupNo", resp.GroupNo),
		zap.Int("seedMembers", len(seed.Members)))
	return resp.GroupNo, nil
}

// renameAllMemberGroup 把全员群名改成项目名（D8）。
//
// 走 UpdateGroupInfo：它已经处理群名截断（MaxGroupNameLen）、版本号推进、
// 以及给客户端下发群信息变更。截断规则属于群，所以项目侧传原始项目名即可。
//
// OperatorUID 传 group.creator，即**建这个群的人**（对全员群就是补建那一刻的项目
// owner），不是"当前群主"——这列只记录建群人，不随群内角色变化改写。传一个系统
// uid 会声称是机器人改的，那更差；这条变更的真正发起者是"项目改名"本身，而建群人
// 是群面上离它最近的一个真人。
func (g *Group) renameAllMemberGroup(ctx *config.Context, projectID, groupNo, name string) error {
	if projectID == "" || groupNo == "" || name == "" {
		return nil
	}
	groupModel, err := g.db.QueryWithGroupNo(groupNo)
	if err != nil {
		return fmt.Errorf("group: query all-member group for rename: %w", err)
	}
	if groupModel == nil || groupModel.Status == GroupStatusDisband {
		return nil // 无事可做，重试也不会变
	}
	if groupModel.ProjectID != projectID {
		// 这个群已经不属于该项目了。项目侧解析 group_no 的那次读校验过归属，
		// 但那是上一个快照；真正挡住写的是下面 ExpectProjectID 栅栏，
		// 它与写在同一条语句里，没有窗口。两层都在，是因为这一层免掉
		// 一次事务、一次版本号自增和一次全员推送。
		g.Warn("全员群已不属于该项目，跳过改名",
			zap.String("projectId", projectID), zap.String("groupNo", groupNo),
			zap.String("groupProjectId", groupModel.ProjectID))
		return nil
	}
	// 比较**截断后**的名字。
	//
	// 群名上限 50 rune，项目名上限 64。拿未截断的项目名去比已截断的群名，超过 50
	// 的项目名会永远比不相等，于是每一次带 name 的项目更新都推一个新版本、给全体
	// 群成员下发一次群信息变更——一个恒假的幂等判断，正好在名字最长的那些项目上失效。
	//
	// 截断规则本身仍归群侧所有（UpdateGroupInfo 会再截一次）；这里复用同一个上限
	// 只是为了让"要不要改"这个判断问对问题。
	if truncateGroupName(name) == groupModel.Name {
		return nil // 幂等：名字已经对了，不推版本、不惊动客户端
	}
	operatorName := ""
	if creator, err := g.userDB.QueryByUID(groupModel.Creator); err == nil && creator != nil {
		operatorName = creator.Name
	}
	// 群在这中间被解散是正常终局，不是改名失败：D8 的同步是机器驱动的，安静跳过。
	// 人工改名走的是同一个服务方法，那一路会拿到这个错误并回一个 not-found——
	// 处置的差别属于调用方。第八轮 review。
	if err := g.groupService.UpdateGroupInfo(&UpdateGroupInfoServiceReq{
		GroupNo:      groupNo,
		OperatorUID:  groupModel.Creator,
		OperatorName: operatorName,
		Name:         &name,
		// 归属栅栏。上面那次核对读的是事务外的快照，而这条改名要经过
		// UpdateGroupInfo 自己的事务：中间仍有窗口，detach 落在窗口里就会让 D8
		// 的机器驱动改名写进一个刚刚不再属于本项目的群，并给它全体成员推一条
		// 群名变更。栅栏写进 UPDATE 的 WHERE，影响 0 行即 errGroupGoneOrDisbanded，
		// 下面按"安静跳过"处理——与群被解散同一个出口。
		ExpectProjectID: projectID,
	}); err != nil && !errors.Is(err, errGroupGoneOrDisbanded) {
		return err
	}
	return nil
}

// truncateGroupName applies the group-name cap the way UpdateGroupInfo does, so
// the rename hook's idempotence check compares like with like.
//
// Rune-based, not byte-based: MaxGroupNameLen counts characters, and a byte cut
// would split a multi-byte rune — every project name in Chinese is multi-byte.
func truncateGroupName(name string) string {
	runes := []rune(name)
	if len(runes) > MaxGroupNameLen {
		return string(runes[:MaxGroupNameLen])
	}
	return name
}
