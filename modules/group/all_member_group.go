package group

import (
	"errors"
	"fmt"

	"github.com/Mininglamp-OSS/octo-lib/config"
	projectmod "github.com/Mininglamp-OSS/octo-server/modules/project"
	"go.uber.org/zap"
)

// 全员群的群面实现（P2）。
//
// 只反向注册初始建群与后续改名钩子；Project 成员和角色变化不再同步
// 原生群成员/角色，群内权限保持独立。

// registerAllMemberGroupHooks 由 1module.go 在模块构造时调用，与
// registerProjectCascadeSteps 并列。
func (g *Group) registerAllMemberGroupHooks() {
	projectmod.RegisterAllMemberGroupProvisioner(g.provisionAllMemberGroup)
	projectmod.RegisterAllMemberGroupRename(g.renameAllMemberGroup)
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
