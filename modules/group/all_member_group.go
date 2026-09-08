package group

import (
	"errors"
	"fmt"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	projectmod "github.com/Mininglamp-OSS/octo-server/modules/project"
	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
	"github.com/gocraft/dbr/v2"
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
// 第二个参数（注册契约里的 spaceID）**刻意不用**，所以写成 `_`。
//
// 项目侧传进来的是"它认为的" Space，而准入闸门必须按群自己的 space_id 判定——那一份
// 从事务内重读的群行来（txGroup.SpaceID）。用参数就是拿调用方的说法去评判不变量，
// 与下面 projectID 那段是同一条理由。签名保留这个位置是因为它属于共享的钩子契约；
// 不用它这件事写在这里，免得下一个读者以为是漏传了。
func (g *Group) admitToAllMemberGroup(ctx *config.Context, _, groupNo, uid string) error {
	if groupNo == "" || uid == "" {
		return errors.New("group: all-member admission requires group_no and uid")
	}
	version, err := ctx.GenSeq(common.GroupMemberSeqKey)
	if err != nil {
		return fmt.Errorf("group: all-member admission GenSeq: %w", err)
	}

	// 事务外先看一眼，只为在"没活干"的时候不开事务。判定用的那一份在事务里重读，
	// 见下面的 QueryWithGroupNoTx——这一份的 project_id / creator 一律不参与写入。
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

	// 群行在**事务内**重读一次，准入闸门用的是这一份。
	//
	// 上面那次读发生在事务之外，用来决定"要不要干活"；把它的 project_id 直接喂给
	// 闸门就是拿一个事务外的快照去评判 I2。这两次读之间 P1 的 detach 可以把群
	// 变回 Space 直属，于是闸门会按一个**已经不成立**的项目归属放行——虽然结果
	// 是"写进一个刚刚不再属于该项目的群"而不是越权，但闸门的整个意义就是它读的
	// 那一份归属是权威的。PR #855 review 的 Q13。
	txGroup, err := g.db.QueryWithGroupNoTx(tx, groupNo)
	if err != nil {
		return fmt.Errorf("group: all-member admission re-read group: %w", err)
	}
	if txGroup == nil || txGroup.Status == GroupStatusDisband {
		// 事务外读到时还在，现在没了或已解散。与上面同一个判断，同一个处置：
		// 无事可做，不是错误。
		g.Warn("全员群在准入事务内已不可用，跳过入群",
			zap.String("groupNo", groupNo), zap.String("uid", uid))
		return nil
	}

	if err := g.db.admitOrRestoreMembersTx(tx, groupNo, txGroup.SpaceID, txGroup.ProjectID,
		[]MemberAdmission{{
			UID:     uid,
			Version: version,
			Role:    MemberRoleCommon,
			// 邀请人记为项目的创建者语义上不成立（把人加进项目的可能是任一管理员），
			// 记本人则谎称是自助加入。记 group.creator——**建这个群的那个人**，
			// 对全员群而言就是补建那一刻的项目 owner。这与 admitToPresetGroup 选择
			// 记本人是不同的答案，因为那里确实没有人邀请，而这里有：项目把他带进来的。
			//
			// 注意 group.creator 是**建群人**，不是"当前群主"：本仓库没有任何一条
			// 交接路径改写它（handOverGroupCreator 与 ensureAllMemberGroupOwner 都
			// 只动 group_member.role），所以建群人离开项目之后这一列会指向一个不在
			// 群里的 uid。这里接受它——邀请人是一条历史记录，写成"当前群主"反而会
			// 随着交接漂移，而且要为此在准入热路径上多读一次群主行。上一版注释把它
			// 说成"这个群在群面上的负责人"，那是错的。PR #855 第五轮 review 的 Q10。
			InviteUID: txGroup.Creator,
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
		// 裹上 ErrAdmittedButNotSubscribed：行已经提交了，剩下的是 broker 侧的订阅
		// 缺口。调用方必须能把它与"准入事务失败"分开——后者由 I4 扫描 B 报出，
		// 前者扫描 B 结构上看不见。PR #855 第七轮 review 的 P2-3。
		return fmt.Errorf("%w: group: all-member admission IM subscribe: %w",
			projectpkg.ErrAdmittedButNotSubscribed, err)
	}

	// 子区订阅。父频道订阅**不覆盖**子区：WuKongIM 里每个子区是独立频道
	// （channel_id = groupNo____shortId），发言权限查的是那个频道自己的订阅者表。
	//
	// 本模块其它五条准入路径都是这两步配对出现（memberAdd、groupScanJoin、
	// 解除拉黑、Service.AddGroupMembers、event.go），移除侧也是对称的
	// （RemoveGroupMembers → removeUserFromGroupThreads）。这里少了这一步，于是
	// 一个被加进项目的人在全员群的子区里发不了言、也收不到实时消息——而且是永久的，
	// 因为准入器每人只跑一次；I4 扫描 B 看的是 group_member 行，看不见订阅。
	// PR #855 第五轮 review。
	//
	// best-effort，与父频道订阅同一个取舍：失败只记日志（addUsersToGroupThreads
	// 自己记），不让整次入群失败——人已经在群里了。
	g.addUsersToGroupThreads(groupNo, []string{uid})
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

	// 群行在**事务内**重读一次，先确认它此刻确实还是本项目的全员群。
	//
	// 与 admitToAllMemberGroup 上的 Q13 同一个理由，只是换了一个 hook：项目侧的
	// queryAllMemberGroupNo 会校验"群还在、group.project_id 还是本项目"，但那是一次
	// 事务外的无锁读；这中间 P1 的 detach 可以把群变回 Space 直属，于是这次同步会
	// 按一个**已经不成立**的归属去改另一个群的群主——而改群主是改"谁控制这个群"。
	// 上一版这里只按 group_no 读 group_member，从头到尾没问过归属。
	// PR #855 第十轮 review 的 P2-3。
	//
	// 与准入那边一样是非锁定读：它读进本事务的读视图，不是一把锁。真正的互斥来自
	// 下面 group_member 上的 FOR UPDATE。
	txGroup, err := g.db.QueryWithGroupNoTx(tx, groupNo)
	if err != nil {
		return fmt.Errorf("group: re-read group for all-member owner sync: %w", err)
	}
	if txGroup == nil || txGroup.Status == GroupStatusDisband || txGroup.ProjectID != projectID {
		// 群没了、已解散、或已经不属于这个项目。无事可做，不是错误——与准入路径
		// 同一个处置。项目指着一个不可用的群由 I4 扫描 A 报出。
		g.Warn("全员群在群主同步事务内已不可用或已改归属，跳过同步",
			zap.String("projectId", projectID), zap.String("groupNo", groupNo))
		return nil
	}

	// 群主在事务内、行锁下读出。无锁读会与并发的另一次同步（或 P1 的级联交接）
	// 各自看到同一个群主并各自提升一个继任者，群里留下两个 role=creator 的行——
	// handOverGroupCreator 上写着这个失败模式，这里是同一个形状。
	//
	// ORDER BY 是确定性的，而且**多行是要处理的状态，不是要忽略的**：上一版取
	// creators[0] 就往下走，于是"两个群主、而第一个恰好还是项目 owner"这一状态
	// 会在开头那个 early return 上原地不动——永远收敛不掉。而 D7 禁止对全员群
	// 转让、退群、解散，群侧没有任何路径能清理它。PR #855 第五轮 review 的 Q2。
	//
	// 无序读还有第二个代价：谁被当成"当前群主"取决于存储引擎的行序，在副本上
	// 可能不同，也测不了。created_at + uid 与 PickActiveOwner 用的是同一个全序。
	var creators []string
	if _, err := tx.SelectBySql(
		"SELECT uid FROM group_member WHERE group_no=? AND role=? AND is_deleted=0 "+
			"ORDER BY created_at ASC, uid ASC FOR UPDATE",
		groupNo, MemberRoleCreator,
	).Load(&creators); err != nil {
		return fmt.Errorf("group: read all-member group creator: %w", err)
	}
	if len(creators) == 0 {
		// 无主群。P1 的级联在无人可继任时会留下这种状态并已经告警过，这里不再
		// 制造第二条告警，也不擅自指派——指派一个群主是改变谁控制这个群。
		return nil
	}
	// keeper 是这次同步之后**唯一**应当留任的群主。先假定是最元老的那一行，
	// 下面在需要交接时改写它；最后一步把其余的 creator 行统统降级。
	keeper := creators[0]
	promotedTo := ""

	// 项目侧的两问都走 tx，不走连接池。
	//
	// 池上读是**另一份快照**，而这个事务此刻正握着上面那些 group_member 行的
	// FOR UPDATE 锁；用另一份快照的答案去决定这一份快照里的写，是没必要引入的一层
	// 不一致。pkg/project 的两个 helper 为此收成了 dbr.SessionRunner（本仓库既有写法）。
	// PR #855 第五轮 review 的 Q11，也是第一轮 Q6 一直推迟的那一半。
	//
	// 但它**不是**一把锁，上一版注释把话说大了（第七轮 review 的 P2-5）：MemberRole
	// 与 PickActiveOwner 都是非锁定读，只是读进了本事务的读视图，并没有锁住
	// octo_project_member——角色仍然可能在读到与提交之间被改掉。这是刻意的取舍：
	// pkg/project 那条不可提权的不变量本来就禁止往上升，而在这里加 FOR SHARE 会给
	// 声明过的锁序添一条没人分析过的边。事务内读严格好于池上读，仅此而已。
	role, ok, err := projectpkg.MemberRole(tx, projectID, keeper)
	if err != nil {
		return fmt.Errorf("group: read project role of all-member group creator: %w", err)
	}
	if !ok || role != projectRoleOwner {
		successor, err := projectpkg.PickActiveOwner(tx, projectID)
		if err != nil {
			return fmt.Errorf("group: pick project owner for all-member group: %w", err)
		}
		switch {
		case successor == "" || successor == keeper:
			// 项目没有活跃 owner（或目标就是现任）。群主留在原处——交给一个非
			// owner 比留着一个前 owner 更糟。多群主仍要收敛，所以这里不 return。
			//
			// 不必再在 creators 里找 owner：PickActiveOwner 与下面那次查找读的是
			// 同一张表、同一个事务，它返回空就意味着这个项目一个活跃 owner 都没有。
		case containsUID(creators, successor):
			// 继任者**已经**是 creator 行之一，即群里此刻有多个群主而目标就在
			// 其中。不能再提升一次：updateMemberRoleIfLiveTx 对一行本就是 creator
			// 的行影响 0 行，会被下面当成"提升落空"而硬失败——一个本该被收敛的
			// 状态就成了永久报错。改留任他，其余的降级。
			keeper = successor
		default:
			ok, err := g.promoteAllMemberGroupCreatorTx(ctx, tx, groupNo, successor)
			if err != nil {
				return err
			}
			if ok {
				keeper = successor
				promotedTo = successor
			} else {
				// 全员群的成员集合等于项目成员集合，所以一个项目 owner 正常总是在群里；
				// 不在，说明 D12 的那次入群失败了（I4 扫描 B 正盯着这件事）。这种情况下
				// 不把群主交给他——一个不在群里的群主客户端渲染不出来，而且下一次入群成功时
				// 他会以普通成员身份被写回，角色就丢了。
				//
				// 但也**不能**就这么留着 creators[0]：它已经被判定为非 owner，而下面的
				// 收敛会把其余每一行 creator 降级——其中可能正有一个仍然是项目 owner 的人
				// （PickActiveOwner 只返回最元老的那一个，更年轻的 owner 它不返回）。
				// 那样这一次同步会**亲手**把群里唯一合法的群主降掉，只留下一个非 owner；
				// 而 D7 禁止对全员群转让/退群/解散，没有任何扫描盯着"群主是不是项目
				// owner"这条关系，于是这个状态既修不了也看不见。
				//
				// 上一版就是这么写的，第六轮 review 逐行推出了这条路径。规则改成：
				// 留任者从"仍是项目 owner 的 creator"里选，选不出来才退回 creators[0]。
				fallback, ferr := firstActiveProjectOwner(tx, projectID, creators)
				if ferr != nil {
					return ferr
				}
				if fallback != "" {
					keeper = fallback
				}
				g.Warn("全员群群主同步：目标 owner 不在群内，保持群内已有的项目 owner 或原群主（I4 扫描 B 会报出缺口）",
					zap.String("projectId", projectID), zap.String("groupNo", groupNo),
					zap.String("successor", successor), zap.String("keeper", keeper))
			}
		}
	}

	// 收敛到一个群主：除 keeper 外的每一行 creator 都降为普通成员。
	//
	// 降级而不是移出群：他还是项目成员（只是不再是 owner），而全员群的成员集合
	// 等于项目成员集合。
	demoted := make([]string, 0, len(creators))
	for _, uid := range creators {
		if uid == keeper {
			continue
		}
		version, err := ctx.GenSeq(common.GroupMemberSeqKey)
		if err != nil {
			return fmt.Errorf("group: generate demote version: %w", err)
		}
		if err := g.db.UpdateMemberRoleTx(groupNo, uid, MemberRoleCommon, version, tx); err != nil {
			return fmt.Errorf("group: demote former all-member group creator: %w", err)
		}
		demoted = append(demoted, uid)
	}
	if promotedTo == "" && len(demoted) == 0 {
		return nil // 什么都没改，不提交
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("group: commit all-member owner sync: %w", err)
	}
	if promotedTo != "" {
		g.Info("全员群群主已同步为项目 owner",
			zap.String("projectId", projectID), zap.String("groupNo", groupNo),
			zap.String("from", creators[0]), zap.String("to", promotedTo))
	}
	if len(demoted) > 1 || (promotedTo == "" && len(demoted) > 0) {
		// 一次降掉不止一行，或者根本没发生交接却仍有要降的行：群里原本有多个
		// role=creator。这是并发交接留下的痕迹，值得被看见。
		g.Warn("全员群存在多个群主，已收敛为一个",
			zap.String("projectId", projectID), zap.String("groupNo", groupNo),
			zap.String("keeper", keeper), zap.Strings("demoted", demoted))
	}
	return nil
}

// firstActiveProjectOwner 返回 uids 里第一个仍是项目活跃 owner 的 uid，都不是则 ""。
//
// 只在"目标 owner 不在群里"那条分支上调用，也就是收敛即将把其余 creator 全部降级、
// 而留任者本身不是 owner 的时候。uids 是这个群的 creator 行，正常是一行、异常是两行，
// 所以这里的逐个查询是有界的，而且这条分支本身就已经是异常路径。
//
// 走 tx，与本函数其余的项目侧查询同一个理由：答案与写落在同一份读视图里。
// 同样不是锁——见 ensureAllMemberGroupOwner 里那段说明。
func firstActiveProjectOwner(tx *dbr.Tx, projectID string, uids []string) (string, error) {
	for _, uid := range uids {
		role, ok, err := projectpkg.MemberRole(tx, projectID, uid)
		if err != nil {
			return "", fmt.Errorf("group: read project role of creator candidate: %w", err)
		}
		if ok && role == projectRoleOwner {
			return uid, nil
		}
	}
	return "", nil
}

// containsUID 报告 uid 是否在 uids 里。
func containsUID(uids []string, uid string) bool {
	for _, u := range uids {
		if u == uid {
			return true
		}
	}
	return false
}

// promoteAllMemberGroupCreatorTx 在事务内把 successor 提升为群主。
//
// 返回 false 表示他此刻不在群里（成员行不存在或已软删除），此时**什么都没写**，
// 调用方应保持原群主不变。
//
// 继任者的成员行必须在**事务内、行锁下**读出。
//
// 无锁读 + 随后的两次写是这条路径最危险的形状：读到"他在群里"，项目侧的级联在
// 这之后把他的行软删除，于是提升影响 0 行（UpdateMemberRoleTx 的 WHERE 带
// is_deleted=0 且**不报错**），而降级照常执行——群里从此**一个群主都没有**。
// 而 D7 恰好禁止对全员群做转让、退群、解散，所以这个群谁也救不回来。
//
// 兄弟路径 project_cascade.go 的 handOverGroupCreator 早就是这么做的：
// FOR UPDATE 选继任者、用 updateMemberRoleIfLiveTx 拿"到底改没改到行"、
// 改不到就**硬失败**。这里当初两样都没做，等于把那条路径吃过的亏重演一遍。
func (g *Group) promoteAllMemberGroupCreatorTx(
	ctx *config.Context, tx *dbr.Tx, groupNo, successor string,
) (bool, error) {
	var successorLive []int
	if _, err := tx.SelectBySql(
		"SELECT 1 FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0 FOR UPDATE",
		groupNo, successor,
	).Load(&successorLive); err != nil {
		return false, fmt.Errorf("group: lock successor membership: %w", err)
	}
	if len(successorLive) == 0 {
		return false, nil
	}
	version, err := ctx.GenSeq(common.GroupMemberSeqKey)
	if err != nil {
		return false, fmt.Errorf("group: generate successor version: %w", err)
	}
	promoted, err := g.db.updateMemberRoleIfLiveTx(tx, groupNo, successor, MemberRoleCreator, version)
	if err != nil {
		return false, fmt.Errorf("group: promote all-member group creator: %w", err)
	}
	if !promoted {
		// 上面刚在 FOR UPDATE 下确认过这一行是活的，所以走到这里是 bug 而不是竞态——
		// 但它**绝不能**继续往下走到降级。在一次落空的提升之上再降一次级，正是群里
		// 一个群主都不剩的成因，而且全程无声。返回错误让事务回滚，什么都不改。
		return false, fmt.Errorf(
			"group: promote %s as creator of %s affected no live member row", successor, groupNo)
	}
	return true, nil
}

// renameAllMemberGroup 把全员群名改成项目名（D8）。
//
// 走 UpdateGroupInfo：它已经处理群名截断（MaxGroupNameLen）、版本号推进、
// 以及给客户端下发群信息变更。截断规则属于群，所以项目侧传原始项目名即可。
//
// OperatorUID 传 group.creator，即**建这个群的人**（对全员群就是补建那一刻的项目
// owner），不是"当前群主"——那一列不随交接改写，见 admitToAllMemberGroup 里的
// 同一条说明。传一个系统 uid 会声称是机器人改的，那更差；这条变更的真正发起者是
// "项目改名了"这件事本身，而建群人是群面上离它最近的一个真人。
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
		// 这个群已经不是这个项目的了（P1 的 detach）。项目侧解析 group_no 的那次读
		// 校验过归属，但那是**上一个快照**。安静跳过，不是错误。
		//
		// 这一次判断只是提前退出：真正挡住写的是下面 ExpectProjectID 那道栅栏，
		// 它与写在同一条语句里，没有窗口。两层都在，是因为这一层免掉了一次
		// 事务、一次版本号自增和一次全员推送。
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
		// admitToAllMemberGroup 用事务内重读解决同一个问题（Q13），这里用栅栏，
		// 因为写的事务不归本函数所有。PR #855 第十轮 review 的 P2-3。
		ExpectProjectID: projectID,
	}); err != nil && !errors.Is(err, errGroupGoneOrDisbanded) {
		return err
	}
	return nil
}

// projectRoleOwner mirrors modules/project.RoleOwner. modules/group MAY import
// modules/project (the forbidden direction is the other one), but this file
// deliberately reaches for pkg/project instead: the predicate package is what
// keeps the dependency a fact rather than a module, and one constant is not a
// reason to pull the whole module into the group binary's init order.
const projectRoleOwner = 2

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
