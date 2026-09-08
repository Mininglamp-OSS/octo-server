package project

import (
	"fmt"
	"time"

	"github.com/gocraft/dbr/v2"
)

// 全员群那一列的读写原语（D4 / D5）。
//
// # 为什么是租约而不是行锁
//
// 建全员群这件事必须发生在**项目事务提交之后**：钩子要在 modules/group 的表上开
// 自己的事务，还要在提交后调 WuKongIM 建频道，把项目行的排他锁跨到这些上面会让
// 建项目与该项目的每一次群写入互相串行，而且会把一个网络调用关进行锁。
//
// 代价是项目行锁**用不上**了：两个并发的写路径（建项目本身、随后的任何一次加成员）
// 都可能发现 all_member_group_no 为空、都去补建，各自建出一个群。这一对操作跨越了
// 事务边界，所以互斥必须由数据本身承担。
//
// 于是用一次 CAS 认领：谁把 lease_until 从"空或已过期"改成"现在 + 租约"谁去建；
// 影响 0 行的直接跳过。租约到期即可被下一个写路径重新认领，因此进程在建群中途被
// 打死不会把这个项目永久钉在"没有全员群"上。这与 space_member_removal_cleanup 和
// octo_project_member_removal_cleanup 的租约是同一个形状，只是租的是一行上的一个
// 字段而不是一整条工单。

const (
	// allMemberGroupLease 一次补建认领的租约时长。
	//
	// 必须显著长于一次建群的实际耗时：CreateGroup 要写群行、写成员行、提交，然后
	// 调 IMCreateOrUpdateChannel 建频道、发群创建通知。IM 那一段是跨进程的 HTTP
	// 调用，是这里唯一可能慢到分钟级的部分。
	//
	// 也不能太长：租约没到期之前，这个项目的补建就是关着的。取 2 分钟——比一次
	// 正常建群长一个数量级，比一个人从"建项目失败"到"再点一次加成员"的间隔短。
	allMemberGroupLease = 2 * time.Minute
)

// clearStaleAllMemberGroupPointer 把指向"已经不是本项目全员群"的指针清空，返回是否清了。
//
// # 为什么必须有这一步
//
// queryAllMemberGroupNo 会校验群侧（群还在、群的 project_id 还是本项目），所以
// P1 把群 detach 成 Space 直属之后，那个查询正确地答"没有全员群"。但认领用的
// CAS 谓词要求 all_member_group_no 为空串，而指针**并没有被清空**——modules/group
// 不能写 octo_project。
//
// 于是两个谓词对同一件事给出不同答案，补建被卡死在中间：ensureAllMemberGroup 看到
// "没有群"于是继续，claimAllMemberGroupProvision 看到"指针非空"于是永远认领不到，
// 既不建群也不报错。之后每一个新成员的 admitAllMemberGroup 都空操作，I4 扫描 A
// 报着一个谁也修不好的项目——正是补建存在的意义被静默取消。
//
// 这个缺口是上一轮修 queryAllMemberGroupNo 时引入的：那个修复让读侧变严，却没让
// 写侧跟上。清指针把两侧重新对齐。
//
// 谓词与 queryAllMemberGroupNo 互为补集：只在"指针非空、但它指的群已经不合格"时
// 才清。指针为空、或群仍然合格，都影响 0 行。
func (d *DB) clearStaleAllMemberGroupPointer(projectID string) (bool, error) {
	if projectID == "" {
		return false, nil
	}
	result, err := d.session.UpdateBySql(
		"UPDATE octo_project p "+
			"LEFT JOIN `group` g "+
			"  ON g.group_no = p.all_member_group_no COLLATE utf8mb4_general_ci "+
			"  AND g.status <> ? "+
			"  AND g.project_id = p.project_id COLLATE utf8mb4_general_ci "+
			"SET p.all_member_group_no = '', p.all_member_group_lease_until = NULL "+
			"WHERE p.project_id = ? AND p.status = ? "+
			"  AND p.all_member_group_no <> '' AND g.id IS NULL",
		groupStatusDisband, projectID, StatusNormal,
	).Exec()
	if err != nil {
		return false, fmt.Errorf("project: clear stale all-member group pointer: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("project: read stale pointer clear result: %w", err)
	}
	return affected == 1, nil
}

// claimAllMemberGroupProvisionTx 尝试认领"给这个项目建全员群"的活。
//
// 返回 true 表示认领成功，调用方应当去建群，并在成功后调用
// setAllMemberGroupNo 写回；失败或崩溃则什么都不用做，租约到期后别人会接手。
//
// 三个谓词缺一不可：
//   - status：已解散的项目不该再长出群来。
//   - all_member_group_no 为空串：已经有群了就没有活可干。这也是幂等性的来源——
//     写回之后任何后续认领都会影响 0 行。
//   - 租约为空或已过期：正在被别人建的项目不重复建。
//
// 用 tx 而不是 session：调用方（建项目、加成员）此时已经提交了自己的业务事务，
// 这里开的是一个只包含这条 UPDATE 的短事务，不与任何别的锁同时持有。
//
// 返回认领时写下的 deadline，它就是这次认领的**凭据**：释放和写回都拿它当围栏
// （见 releaseAllMemberGroupProvision）。没有它，一次超时的认领在退出时会清掉
// 后继者正握着的租约。
func (d *DB) claimAllMemberGroupProvision(projectID string, now time.Time) (bool, time.Time, error) {
	if projectID == "" {
		return false, time.Time{}, nil
	}
	// 截到毫秒，因为围栏是**等值**比较而列是 DATETIME(3)。
	//
	// 不截的话写进去的是被库截过的值，手里留的是纳秒精度的原值，两者永远不相等：
	// 释放和写回的围栏会静默影响 0 行，租约只能等自然到期——而"围栏永远不匹配"
	// 与"没有围栏"在日志上长得一模一样。是 TestReleasingAStaleClaimDoesNotClear...
	// 的最后一条断言发现的，不是想出来的。
	deadline := now.Add(allMemberGroupLease).Truncate(time.Millisecond)
	result, err := d.session.UpdateBySql(
		"UPDATE octo_project SET all_member_group_lease_until = ? "+
			"WHERE project_id = ? AND status = ? AND all_member_group_no = '' "+
			"  AND (all_member_group_lease_until IS NULL OR all_member_group_lease_until < ?)",
		deadline, projectID, StatusNormal, now,
	).Exec()
	if err != nil {
		return false, time.Time{}, fmt.Errorf("project: claim all-member group provision: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, time.Time{}, fmt.Errorf("project: read all-member group claim result: %w", err)
	}
	if affected != 1 {
		return false, time.Time{}, nil
	}
	return true, deadline, nil
}

// releaseAllMemberGroupProvision 主动放弃认领（建群失败时），把租约清空让下一个
// 写路径立刻可以重试，而不必等满一个租约周期。
//
// # 必须围栏在自己认领的那个 deadline 上
//
// 前一版的 WHERE 只有「project_id 且 all_member_group_no 仍为空」，也就是**谁的
// 租约都清**。一次跑得比 allMemberGroupLease 还久的建群尝试，在失败退出时会清掉
// 一个后继者刚刚认领、正在使用的租约——于是第三个写路径立刻认领成功，两个建群
// 并发跑起来，而这正是租约存在的全部理由。（对称的另一半由 setAllMemberGroupNo
// 的 CAS 兜住：晚到的那次写回落空，多出来的群留成普通项目群。）
//
// 加上 deadline 之后，超时者的释放影响 0 行：它手里的凭据已经不是行上的那个值。
// 这与 completeRemovalJob 用 lease owner 做围栏是同一个手法——那里是"这个工单
// 还归我吗"，这里是"这一列上的租约还是我写的那个吗"。
//
// best-effort：失败不影响正确性，租约到期同样会释放。
func (d *DB) releaseAllMemberGroupProvision(projectID string, deadline time.Time) error {
	if projectID == "" || deadline.IsZero() {
		return nil
	}
	_, err := d.session.UpdateBySql(
		"UPDATE octo_project SET all_member_group_lease_until = NULL "+
			"WHERE project_id = ? AND all_member_group_no = '' "+
			"  AND all_member_group_lease_until = ?",
		projectID, deadline,
	).Exec()
	if err != nil {
		return fmt.Errorf("project: release all-member group provision: %w", err)
	}
	return nil
}

// setAllMemberGroupNo 把建好的群写回项目行并清空租约，围栏在本次认领的 deadline 上。
//
// # 为什么"指针仍为空串"这一条不够
//
// 前一版只有那一条谓词，理由写的是：租约会过期，别人可能已经接手并建成，这时后到的
// 写回必须落空。那段论证只考虑了**后继者先写回**的那个顺序，而它恰好是自洽的——指针
// 已非空，晚到的一方自然落空。
//
// 反过来的顺序没被考虑，而它才是有害的那个：
//
//	A 认领（deadline Ta）→ A 的 CreateGroup 跑得很久 → Ta 过期
//	→ B 认领（Tb）、读**当前**名册、建出 Gb
//	→ A 回来写回 Ga（此时指针仍是空串，于是**成功**）
//	→ B 写回 Gb，落空
//
// 结果是项目指向 Ga —— A 那份更旧的名册快照建出来的群，窗口期内加进项目的人一个都
// 不在里面；而带着完整名册的 Gb 留成一个普通项目群，于是这个 Space 里出现两个以项目
// 命名的群。缺的那些人是 I4 缺口，扫描 B 过了宽限期才报，而且没有自动修复。
//
// 租约超时不是异常路径：allMemberGroupLease 的注释自己说，IM 建频道是"这里唯一可能
// 慢到分钟级的部分"，2 分钟是按它的数量级取的——超出它正是租约被设计出来要处理的情况。
//
// # 为什么围栏是 deadline 的**等值**比较
//
// 直觉上会担心它把良性情况也挡掉：一次超时但**没人接手**的认领怎么办？不会挡——那种
// 情况下行上的 lease_until 仍然是这个 claimant 自己写下的那个值，等值比较成立，写回照常
// 落地。只有真的出现了后继者（行上的值被换成 Tb）才会落空，而那时该赢的本来就是 B。
// 两个方向都对，这是选等值而不是"检查新鲜度"的理由。
//
// 返回 false 表示本次写回落空，调用方应当记日志——它建出来的那个群会作为普通项目群
// 留存（group.project_id 已经指向本项目），由 I4 扫描 A 报出来。
func (d *DB) setAllMemberGroupNo(projectID, groupNo string, deadline time.Time) (bool, error) {
	if projectID == "" || groupNo == "" || deadline.IsZero() {
		return false, nil
	}
	result, err := d.session.UpdateBySql(
		"UPDATE octo_project SET all_member_group_no = ?, all_member_group_lease_until = NULL "+
			"WHERE project_id = ? AND status = ? AND all_member_group_no = '' "+
			"  AND all_member_group_lease_until = ?",
		groupNo, projectID, StatusNormal, deadline,
	).Exec()
	if err != nil {
		return false, fmt.Errorf("project: set all-member group: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("project: read set all-member group result: %w", err)
	}
	return affected == 1, nil
}

// clearAllMemberGroupNoTx 在解散事务内清空全员群归属（D10）。
//
// 群本身不动：P1 的解散级联会把它连同该项目的其它群一起回退成 Space 直属
// （project_id 置为空串），成员原样保留。这里清的只是"哪个群是全员群"这个事实——
// 项目已经不存在了，这个事实也就不再成立。
//
// 在解散事务内、与 status 翻转同时提交，而不是交给级联步骤：级联步骤失败是允许的
// （P1 明确不因此回滚解散），而一个已解散项目却还指着某个群，会让 D7 的保护谓词
// 和补建谓词都要多考虑一种状态。清在事务内则这种状态不存在。
func (d *DB) clearAllMemberGroupNoTx(tx *dbr.Tx, projectID string) error {
	if projectID == "" {
		return nil
	}
	_, err := tx.UpdateBySql(
		"UPDATE octo_project SET all_member_group_no = '', all_member_group_lease_until = NULL "+
			"WHERE project_id = ?",
		projectID,
	).Exec()
	if err != nil {
		return fmt.Errorf("project: clear all-member group: %w", err)
	}
	return nil
}

// queryAllMemberGroupNo 读一个活跃项目**当前仍然拥有**的全员群号。
//
// 返回 "" 表示没有：项目不存在、已解散、尚未建成，或者那个群已经不属于本项目了。
// 四者对调用方是同一个答案——这个项目现在没有全员群可操作。
//
// # 为什么必须同时校验群侧
//
// 只读 all_member_group_no 是不够的，而且不够的方式是**静默**的。P1 的成员移除
// 级联在群主离开项目、且项目里没人能继任时，会把群回退成 Space 直属
// （project_id 置空），并且**不会**清掉项目这一侧的指针——modules/group 不能写
// octo_project。I4 扫描 A 把这种状态明确记作"指向已解散或已脱离的群"。
//
// 若这里不校验，那之后：
//   - 新加入项目的人会被塞进一个已经与项目无关的群（admitAllMemberGroup）；
//   - 项目改名会去改那个群的名字（syncAllMemberGroupName）；
//   - 群主同步会去动那个群的群主（syncAllMemberGroupOwner）。
//
// 三件都是对一个"别人的群"的写入。pkg/project.IsAllMemberGroup 早就是两半都查的，
// D7 的保护因此正确；这里当初只查了一半，两个谓词对同一个问题给出不同答案。
func (d *DB) queryAllMemberGroupNo(projectID string) (string, error) {
	if projectID == "" {
		return "", nil
	}
	// 两条单表查询，不是一次连接 —— 与 pkg/project.IsAllMemberGroup 同一个理由，
	// 那里有实测：显式 COLLATE 的 coercibility 是 0，所以比较落在 general_ci，
	// 而生产里 0900_ai_ci 的 `group`.group_no 必须逐行转换，group_groupNo 用不上，
	// 计划从 const 退化成全表扫。跨过 schema 的比较一旦不存在，就不需要对排序规则
	// 有任何意见，转换落地之后也不用回来改。
	//
	// 这条查询在加人批次的开头、群主同步、改名同步上各跑一次，都是写路径。
	var pointers []string
	if _, err := d.session.SelectBySql(
		"SELECT all_member_group_no FROM `octo_project` "+
			"WHERE project_id = ? AND status = ? AND all_member_group_no <> ''",
		projectID, StatusNormal,
	).Load(&pointers); err != nil {
		return "", fmt.Errorf("project: query all-member group pointer: %w", err)
	}
	if len(pointers) == 0 || pointers[0] == "" {
		return "", nil
	}
	// 群侧那一半：群还在、没解散、而且**还属于本项目**。缺了它，一个被 P1 detach
	// 成 Space 直属的群仍会被当成全员群使用（见本函数上方的注释）。
	var alive []int
	if _, err := d.session.SelectBySql(
		"SELECT 1 FROM `group` WHERE group_no = ? AND status <> ? AND project_id = ?",
		pointers[0], groupStatusDisband, projectID,
	).Load(&alive); err != nil {
		return "", fmt.Errorf("project: query all-member group row: %w", err)
	}
	if len(alive) == 0 {
		return "", nil
	}
	return pointers[0], nil
}

// queryActiveOwnerCandidatesForProvision 按资历返回项目的活跃 owner 候选（最老在前）。
//
// 补建全员群时用来决定"谁当群主"。不能用 octo_project.creator：那一列记的是
// 当初是谁建的项目，**永不改变**，而这个人可能早就离开了项目或 Space。用他去建群
// 会被准入闸门当场拒掉（他不是项目活跃成员），于是一个初次建群失败过的项目
// **永远**补建不出来——每一次写路径都认领租约、建群失败、释放租约，循环到底。
//
// 选人规则与 pkg/project.PickActiveOwner 一致（资历最老），刻意同源：那个是群侧
// 群主同步用的，两边对"谁该拥有这个群"必须给出同一个答案，否则补建刚建好，
// 群主同步就把它改掉。
//
// # 为什么是候选**列表**而不是一个人
//
// 前一版只取 LIMIT 1，把那个 uid 直接交给 CreateGroup —— 而 CreateGroup 的第一件事
// 就是 CheckMembership(space, creator)：项目席位不蕴含 Space 席位。资历最老的那位
// owner 一旦成了 I1 泄漏（项目席位还活着、Space 席位没了，P0 的级联工单放弃后就是
// 这个状态，i1_abandoned_cleanup_leak 这个指标就是为它设的），补建就会失败——而且
// 因为这个选择是**确定性全序**，它会以完全相同的方式失败每一次，哪怕项目里还有
// 别的 owner 本可以通过。一个只报不修的扫描 A，加上一条永远修不好的补建路径。
//
// 返回有序候选，由调用方按 Space 席位筛，是把"谁能当群主"这个判断交给会真正校验它
// 的那个谓词。
//
// limit 由调用方按 max_members 传入，而不是一个自选的小常数。前一版写死 16，而没有
// 任何东西把一个项目的 owner 数量限制在 16 以内——如果最资深的 16 位恰好都是 I1 泄漏
// 而第 17 位持有席位，补建会以完全相同的方式失败每一次，也就是刚修掉的那个缺陷的
// 窄版本。owner 是成员的子集，所以成员配额就是这里天然的、有意义的界。
func (d *DB) queryActiveOwnerCandidatesForProvision(projectID string, limit int) ([]string, error) {
	if projectID == "" || limit <= 0 {
		return nil, nil
	}
	var uids []string
	_, err := d.session.SelectBySql(
		"SELECT uid FROM `octo_project_member` "+
			"WHERE project_id = ? AND role = ? AND status = ? AND removing = 0 "+
			// created_at 不是全序（同毫秒会并列），补 uid 让选择可测且跨副本一致。
			"ORDER BY created_at ASC, uid ASC LIMIT ?",
		projectID, RoleOwner, MemberStatusActive, limit,
	).Load(&uids)
	if err != nil {
		return nil, fmt.Errorf("project: query active owner candidates: %w", err)
	}
	return uids, nil
}

// queryActiveMemberUIDsForRebuild 读一个项目当前的活跃成员 uid，供 D4 补建把整份
// 名册当作建群的初始成员（见 ensureAllMemberGroup）。
//
// 排除 removing = 1：那些席位正在关闭，级联马上会把他们从项目的每个群里移走，
// 把他们放进新群等于建出来就要再拆掉一次。
//
// 不排除系统 bot：它们本来就不持有项目席位（I2/I4 都豁免它们），所以这条查询
// 读不到它们，不需要额外的谓词。
//
// limit 由调用方传入，而且调用方要传 **max_members + 1**，不是 max_members。
//
// 因为这个函数只负责有界，判断"有没有被截断"是调用方的事，而它只能通过"拿回来的
// 行数超过了配额"来判断——如果 limit 就等于配额，那个条件按构造永远不成立，检查
// 变成死代码。上一轮就是这么错的：nit 说 `>=` 会在项目正好满员时误报，我只把比较符
// 改成 `>`，于是唯一的信号没了，截断变成静默的。
//
// 截断本身是可达的：updateProject 允许调小 max_members，且不校验当前活跃席位数。
//
// 有界是硬要求而不是防御：这条语句在一次 HTTP 请求里同步执行。
func (d *DB) queryActiveMemberUIDsForRebuild(projectID string, limit int) ([]string, error) {
	var uids []string
	_, err := d.session.SelectBySql(
		// 按 created_at, uid 排序而不是随便什么顺序：建群时的成员顺序会决定
		// group_member 的写入顺序，稳定的顺序让两次补建产生一样的结果，测试
		// 才断言得了。
		"SELECT uid FROM `octo_project_member` "+
			"WHERE project_id = ? AND status = ? AND removing = 0 "+
			"ORDER BY created_at, uid LIMIT ?",
		projectID, MemberStatusActive, limit,
	).Load(&uids)
	if err != nil {
		return nil, fmt.Errorf("project: query active member uids for rebuild: %w", err)
	}
	return uids, nil
}
