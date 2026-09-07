package group

import (
	"errors"
	"fmt"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	projectmod "github.com/Mininglamp-OSS/octo-server/modules/project"
	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
	"go.uber.org/zap"
)

// 全员群的群面实现（P2）。
//
// 四个钩子，全部反向注册进 modules/project——方向与 project_cascade.go 一样，
// 理由也一样：modules/project 永不 import modules/group。
//
// 每个都必须**幂等**：项目侧会在补建和重试路径上重复调用它们。

// registerAllMemberGroupHooks 由 1module.go 在模块构造时调用，与
// registerProjectCascadeSteps 并列。
func (g *Group) registerAllMemberGroupHooks() {
	projectmod.RegisterAllMemberGroupProvisioner(g.provisionAllMemberGroup)
	projectmod.RegisterAllMemberGroupAdmitter(g.admitToAllMemberGroup)
	projectmod.RegisterAllMemberGroupOwnerTransfer(g.ensureAllMemberGroupOwner)
	projectmod.RegisterAllMemberGroupRename(g.renameAllMemberGroup)
}

// provisionAllMemberGroup 为一个项目建出全员群，返回 group_no。
//
// # 走 CreateGroup，不自己拼 INSERT
//
// 标准建群路径承担的东西，逐一都是"自己写一遍必然漏掉"的那类：唯一准入口
// （于是 I2 在建群事务内被强制）、IM 频道创建与失败补偿、群创建通知、
// bot_admin 处理、以及外部成员标记。project_cascade.go 对 RemoveGroupMembers
// 说过同一段话，这里是它的建群侧对偶。
//
// # 谁是初始成员
//
// seed.Members 只含建项目时勾选的分身；创建者由 CreateGroup 自己作为群主加入。
// 分身此刻**已经**是项目的活跃成员——它们的席位在建项目那个事务里写下并提交了，
// 而这个钩子在提交之后才被调用。于是准入闸门会放行它们，而如果席位因为任何原因
// 没写成，闸门会拒绝，整个建群失败，项目落到"没有全员群"的状态由 D4 兜底。
// 这个顺序不是巧合，是让不变量而不是调用顺序来回答"分身进得来吗"。
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
		Members:   seed.Members,
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

// admitToAllMemberGroup 把一个 uid 放进全员群（D12）。
//
// 与 admitToPresetGroup 是同一个形状，而且是**刻意**的复制而不是抽取成一个共享
// 函数：两者的 invite_uid 语义不同（预设群是"自助加入"，这里是"项目成员资格
// 带进来的"），entry 标签不同（准入拒绝按入口分类的价值就在于分得开），
// 未来的分歧点也不同。把它们合并会让第一次分歧变成一个带 flag 的函数。
//
// projectID 从群行读出，而不是由调用方传：项目侧传进来的是"它认为的"归属，
// 而闸门要按群自己的归属判定。P1 在 admitToPresetGroup 上写过同一段理由——
// 传空串会变成一个 fail-OPEN 的捷径（空 project_id 读作"不是项目群"）。
func (g *Group) admitToAllMemberGroup(ctx *config.Context, spaceID, groupNo, uid string) error {
	if groupNo == "" || uid == "" {
		return errors.New("group: all-member admission requires group_no and uid")
	}
	version, err := ctx.GenSeq(common.GroupMemberSeqKey)
	if err != nil {
		return fmt.Errorf("group: all-member admission GenSeq: %w", err)
	}

	groupModel, err := g.db.QueryWithGroupNo(groupNo)
	if err != nil {
		return fmt.Errorf("group: all-member admission query group: %w", err)
	}
	if groupModel == nil {
		return fmt.Errorf("group: all-member admission: group %s not found", groupNo)
	}
	if groupModel.Status == GroupStatusDisband {
		// 群已解散。不是错误，是无事可做——重试也不会变，返回 error 只会让调用方
		// 反复记一条同样的日志。I4 扫描 A 会把"项目指着一个已解散的群"报出来。
		g.Warn("全员群已解散，跳过入群",
			zap.String("groupNo", groupNo), zap.String("uid", uid))
		return nil
	}

	tx, err := ctx.DB().Begin()
	if err != nil {
		return fmt.Errorf("group: all-member admission begin: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	if err := g.db.admitOrRestoreMembersTx(tx, groupNo, groupModel.SpaceID, groupModel.ProjectID,
		[]MemberAdmission{{
			UID:     uid,
			Version: version,
			Role:    MemberRoleCommon,
			// 邀请人记为项目的创建者语义上不成立（把人加进项目的可能是任一管理员），
			// 记本人则谎称是自助加入。记群主——他是这个群在群面上的负责人，也是
			// 这个群存在的原因。这与 admitToPresetGroup 选择记本人是不同的答案，
			// 因为那里确实没有人邀请，而这里有：项目把他带进来的。
			InviteUID: groupModel.Creator,
		}}, AdmissionEntryAllMemberGroup); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("group: all-member admission commit: %w", err)
	}

	// IM 订阅在提交之后，与其它每一条准入路径一样：这是一次阻塞的 HTTP 调用，
	// 不能握在事务里。失败意味着人在群里但收不到消息——与全仓库其它准入路径同样的
	// 暴露面，不在这里重新解决（#797）。返回错误让调用方记日志而不是谎称成功。
	if err := ctx.IMAddSubscriber(&config.SubscriberAddReq{
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Subscribers: []string{uid},
	}); err != nil {
		g.Error("全员群 IM 订阅失败，成员已入库但收不到消息",
			zap.String("groupNo", groupNo), zap.String("uid", uid), zap.Error(err))
		return fmt.Errorf("group: all-member admission IM subscribe: %w", err)
	}
	return nil
}

// ensureAllMemberGroupOwner 确保全员群的群主是本项目的活跃 owner（D6）。
//
// 自决且幂等：项目侧在任何一次 owner 变动之后都会无条件调用它，包括那些其实
// 没有影响群主的变动。判定必须在这一侧，因为"群主是谁"是群的状态；项目侧
// 传一个"新 owner"进来会在多 owner 的项目上给出错误答案（见注册点上的注释）。
//
// 无可交接时**什么都不做**，不是错误：一个没有活跃 owner 的项目是可达状态
// （P0 的 Space 级联会关掉唯一 owner 的席位并留下一条 Warn），把群主留在原处
// 比交给一个非 owner 更好。
func (g *Group) ensureAllMemberGroupOwner(ctx *config.Context, projectID, groupNo string) error {
	if projectID == "" || groupNo == "" {
		return nil
	}
	tx, err := ctx.DB().Begin()
	if err != nil {
		return fmt.Errorf("group: begin all-member owner sync: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	// 群主在事务内、行锁下读出。无锁读会与并发的另一次同步（或 P1 的级联交接）
	// 各自看到同一个群主并各自提升一个继任者，群里留下两个 role=creator 的行——
	// handOverGroupCreator 上写着这个失败模式，这里是同一个形状。
	var creators []string
	if _, err := tx.SelectBySql(
		"SELECT uid FROM group_member WHERE group_no=? AND role=? AND is_deleted=0 FOR UPDATE",
		groupNo, MemberRoleCreator,
	).Load(&creators); err != nil {
		return fmt.Errorf("group: read all-member group creator: %w", err)
	}
	if len(creators) == 0 {
		// 无主群。P1 的级联在无人可继任时会留下这种状态并已经告警过，这里不再
		// 制造第二条告警，也不擅自指派——指派一个群主是改变谁控制这个群。
		return nil
	}
	currentCreator := creators[0]

	role, ok, err := projectpkg.MemberRole(ctx.DB(), projectID, currentCreator)
	if err != nil {
		return fmt.Errorf("group: read project role of all-member group creator: %w", err)
	}
	if ok && role == projectRoleOwner {
		return nil // 群主仍是项目 owner，无事可做
	}

	successor, err := projectpkg.PickActiveOwner(ctx.DB(), projectID)
	if err != nil {
		return fmt.Errorf("group: pick project owner for all-member group: %w", err)
	}
	if successor == "" || successor == currentCreator {
		return nil
	}

	// 继任者必须已经在群里。全员群的成员集合等于项目成员集合，所以一个项目 owner
	// 正常总是在群里；不在，说明 D12 的那次入群失败了（I4 扫描 B 正盯着这件事）。
	// 这种情况下不把群主交给他——一个不在群里的群主，客户端渲染不出来，而且下一次
	// 入群成功时他会以普通成员身份被写回，角色就丢了。留在原处，等扫描报出来。
	successorMember, err := g.db.QueryMemberWithUID(successor, groupNo)
	if err != nil {
		return fmt.Errorf("group: query successor membership: %w", err)
	}
	if successorMember == nil || successorMember.IsDeleted == 1 {
		g.Warn("全员群群主同步：目标 owner 不在群内，保持原群主不变（I4 扫描 B 会报出缺口）",
			zap.String("projectId", projectID), zap.String("groupNo", groupNo),
			zap.String("successor", successor))
		return nil
	}

	successorVersion, err := ctx.GenSeq(common.GroupMemberSeqKey)
	if err != nil {
		return fmt.Errorf("group: generate successor version: %w", err)
	}
	if err := g.db.UpdateMemberRoleTx(groupNo, successor, MemberRoleCreator, successorVersion, tx); err != nil {
		return fmt.Errorf("group: promote all-member group creator: %w", err)
	}
	demoteVersion, err := ctx.GenSeq(common.GroupMemberSeqKey)
	if err != nil {
		return fmt.Errorf("group: generate demote version: %w", err)
	}
	// 原群主降为普通成员，而不是移出群：他还是项目成员（他只是不再是 owner），
	// 而全员群的成员集合等于项目成员集合。
	if err := g.db.UpdateMemberRoleTx(groupNo, currentCreator, MemberRoleCommon, demoteVersion, tx); err != nil {
		return fmt.Errorf("group: demote former all-member group creator: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("group: commit all-member owner sync: %w", err)
	}
	g.Info("全员群群主已同步为项目 owner",
		zap.String("projectId", projectID), zap.String("groupNo", groupNo),
		zap.String("from", currentCreator), zap.String("to", successor))
	return nil
}

// renameAllMemberGroup 把全员群名改成项目名（D8）。
//
// 走 UpdateGroupInfo：它已经处理群名截断（MaxGroupNameLen）、版本号推进、
// 以及给客户端下发群信息变更。截断规则属于群，所以项目侧传原始项目名即可。
//
// OperatorUID 传群主：改名会产生一条可见的变更，而"项目改名了"这件事在群里
// 最贴切的归属人就是这个群的负责人。传一个系统 uid 会声称是机器人改的。
func (g *Group) renameAllMemberGroup(ctx *config.Context, groupNo, name string) error {
	if groupNo == "" || name == "" {
		return nil
	}
	groupModel, err := g.db.QueryWithGroupNo(groupNo)
	if err != nil {
		return fmt.Errorf("group: query all-member group for rename: %w", err)
	}
	if groupModel == nil || groupModel.Status == GroupStatusDisband {
		return nil // 无事可做，重试也不会变
	}
	if groupModel.Name == name {
		return nil // 幂等：名字已经对了，不推版本、不惊动客户端
	}
	operatorName := ""
	if creator, err := g.userDB.QueryByUID(groupModel.Creator); err == nil && creator != nil {
		operatorName = creator.Name
	}
	return g.groupService.UpdateGroupInfo(&UpdateGroupInfoServiceReq{
		GroupNo:      groupNo,
		OperatorUID:  groupModel.Creator,
		OperatorName: operatorName,
		Name:         &name,
	})
}

// projectRoleOwner mirrors modules/project.RoleOwner. modules/group MAY import
// modules/project (the forbidden direction is the other one), but this file
// deliberately reaches for pkg/project instead: the predicate package is what
// keeps the dependency a fact rather than a module, and one constant is not a
// reason to pull the whole module into the group binary's init order.
const projectRoleOwner = 2
