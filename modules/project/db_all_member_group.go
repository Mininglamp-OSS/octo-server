package project

import (
	"fmt"
	"time"

	"github.com/gocraft/dbr/v2"
)

// 全员群的建群事务提交后读写原语（初始 provisioning）。
//
// 建群钩子在项目事务提交之后运行：它要在 modules/group 的表上开自己的事务，还要在
// 提交后调用 WuKongIM 建频道。把这些放进项目事务会跨模块持有项目锁，并把网络调用
// 关进锁里。
//
// 初始建群失败不回滚 Project；lease 只防止同一个 Project 的补偿/重复触发并发创建，
// 直到某次尝试成功写回 all_member_group_no。成员增删和角色变更不触发这里，也不改
// 原生 group_member。

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

// queryAllMemberGroupNo's two statements, kept as constants so the rename path
// executes the exact SQL covered by its collation guard.
const (
	sqlProjectAllMemberGroupPointer = "SELECT all_member_group_no FROM `octo_project` " +
		"WHERE project_id = ? AND status = ? AND all_member_group_no <> ''"
	sqlProjectAllMemberGroupRow = "SELECT 1 FROM `group` " +
		"WHERE group_no = ? AND status <> ? AND project_id = ?"
)

// queryAllMemberGroupNo reads the active Project's currently associated native
// all-member group number. It is used only by the metadata rename hook.
//
// A blank result means the Project is absent, disbanded, not provisioned, or the
// native group no longer points back at this Project. The rename hook treats all
// of those states as "nothing to rename".
func (d *DB) queryAllMemberGroupNo(projectID string) (string, error) {
	if projectID == "" {
		return "", nil
	}
	var pointers []string
	if _, err := d.session.SelectBySql(
		sqlProjectAllMemberGroupPointer, projectID, StatusNormal,
	).Load(&pointers); err != nil {
		return "", fmt.Errorf("project: query all-member group pointer: %w", err)
	}
	if len(pointers) == 0 || pointers[0] == "" {
		return "", nil
	}
	var alive []int
	if _, err := d.session.SelectBySql(
		sqlProjectAllMemberGroupRow,
		pointers[0], groupStatusDisband, projectID,
	).Load(&alive); err != nil {
		return "", fmt.Errorf("project: query all-member group row: %w", err)
	}
	if len(alive) == 0 {
		return "", nil
	}
	return pointers[0], nil
}
