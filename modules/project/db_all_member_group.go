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
func (d *DB) claimAllMemberGroupProvision(projectID string, now time.Time) (bool, error) {
	if projectID == "" {
		return false, nil
	}
	result, err := d.session.UpdateBySql(
		"UPDATE octo_project SET all_member_group_lease_until = ? "+
			"WHERE project_id = ? AND status = ? AND all_member_group_no = '' "+
			"  AND (all_member_group_lease_until IS NULL OR all_member_group_lease_until < ?)",
		now.Add(allMemberGroupLease), projectID, StatusNormal, now,
	).Exec()
	if err != nil {
		return false, fmt.Errorf("project: claim all-member group provision: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("project: read all-member group claim result: %w", err)
	}
	return affected == 1, nil
}

// releaseAllMemberGroupProvision 主动放弃认领（建群失败时），把租约清空让下一个
// 写路径立刻可以重试，而不必等满一个租约周期。
//
// 只在 all_member_group_no 仍为空时清：如果这中间有别人建成了，那一列已经非空，
// 清租约就成了对别人成果的干扰（虽然实际无害，但让语义变模糊）。
// best-effort：失败不影响正确性，租约到期同样会释放。
func (d *DB) releaseAllMemberGroupProvision(projectID string) error {
	if projectID == "" {
		return nil
	}
	_, err := d.session.UpdateBySql(
		"UPDATE octo_project SET all_member_group_lease_until = NULL "+
			"WHERE project_id = ? AND all_member_group_no = ''",
		projectID,
	).Exec()
	if err != nil {
		return fmt.Errorf("project: release all-member group provision: %w", err)
	}
	return nil
}

// setAllMemberGroupNo 把建好的群写回项目行并清空租约。
//
// WHERE 里那句「all_member_group_no 仍为空串」是这里唯一防止"两个群都写进去、后者覆盖前者"的
// 东西。理论上认领已经保证了只有一个人走到这里，但认领租约会过期：一个跑了超过
// allMemberGroupLease 的建群操作，其租约可能已经被别人接走并且别人已经建成了。
// 这时后到的那次写回必须落空——它建出来的那个群会变成一个普通项目群（group.project_id
// 已经指向本项目），由 I4 扫描 A 报出来，而不是把已经生效的全员群顶掉。
//
// 返回 false 即表示发生了上面这种情况，调用方应当记日志。
func (d *DB) setAllMemberGroupNo(projectID, groupNo string) (bool, error) {
	if projectID == "" || groupNo == "" {
		return false, nil
	}
	result, err := d.session.UpdateBySql(
		"UPDATE octo_project SET all_member_group_no = ?, all_member_group_lease_until = NULL "+
			"WHERE project_id = ? AND status = ? AND all_member_group_no = ''",
		groupNo, projectID, StatusNormal,
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

// queryAllMemberGroupNo 读一个活跃项目的全员群号。返回 "" 表示没有（项目不存在、
// 已解散，或尚未建成）——三者对调用方是同一个答案：这个项目现在没有全员群。
func (d *DB) queryAllMemberGroupNo(projectID string) (string, error) {
	if projectID == "" {
		return "", nil
	}
	var groupNos []string
	_, err := d.session.SelectBySql(
		"SELECT all_member_group_no FROM `octo_project` WHERE project_id = ? AND status = ?",
		projectID, StatusNormal,
	).Load(&groupNos)
	if err != nil {
		return "", fmt.Errorf("project: query all-member group: %w", err)
	}
	if len(groupNos) == 0 {
		return "", nil
	}
	return groupNos[0], nil
}
