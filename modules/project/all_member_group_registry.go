package project

import (
	"sync"

	"github.com/Mininglamp-OSS/octo-lib/config"
)

// 全员群的四个反向注册点（建群、准入、群主同步、改名）。
//
// # 为什么又是反向注册
//
// 与 cascade_registry.go 完全相同的理由，这里不重复论证，只重复结论：
// modules/project 永不 import modules/group（pkg/project/import_guard_test.go 钉着
// 两个方向都为 0），所以群面的活由 modules/group 在构造时注册进来。
//
// # 为什么全部在项目事务**提交之后**运行
//
// 每个钩子都要在 modules/group 的表上开自己的事务，provisioner 还要在它自己的事务
// 提交后调 WuKongIM 建频道。把这些放进项目事务意味着：
//
//   - 项目行的排他锁会被跨模块地持有到另一个模块的事务结束，于是建项目 / 加成员
//     与该项目的每一次群写入互相串行；
//   - 一个跨进程的 HTTP 调用被关进行锁里。
//
// modules/project/service.go 的 runDisbandSteps 上写着同一段理由，P1 就是这么定的。
//
// The accepted failure mode is explicit:
//
//   - provisioner 失败 → 项目已创建、all_member_group_no 为空串。初次建群失败不会回滚
//     Project；I4 扫描 A 会报告缺失产物，供运维处理。
//   - rename 同步失败 → 群名与项目不一致。只记日志加指标。
//
// # 契约
//
// 与 cascade_registry.go 的步骤契约一致，且都必须**幂等**。

// AllMemberGroupSeed 是一次建群请求。
type AllMemberGroupSeed struct {
	ProjectID string
	SpaceID   string
	// Creator 是项目的 owner，也会成为群主（D6）。
	Creator string
	// Name 是项目名。群侧负责按 group 的上限截断——截断规则属于群，不属于项目。
	Name string
	// Members 是除 Creator 之外的初始成员（建项目时勾选的分身）。可以为空：
	// 只有一个人的全员群是完全正常的，这也是 Service.CreateGroup 必须放宽
	// 「members 不能为空」的原因。
	Members []string
}

// AllMemberGroupProvisioner 建出一个属于该项目的全员群，返回 group_no。
//
// 实现必须走 modules/group 的标准建群路径（Service.CreateGroup 带 project_id），
// 而不是自己拼 INSERT：标准路径承担 IM 频道创建、群创建通知、以及 I2 准入闸门。
// 准入闸门是关键——它会在建群事务内确认每个初始成员都是本项目的活跃成员，于是
// 「分身的项目席位写进去了吗」这件事由不变量本身回答，而不是由调用顺序回答。
type AllMemberGroupProvisioner func(ctx *config.Context, seed AllMemberGroupSeed) (string, error)

// AllMemberGroupAdmitter ensures uid is present in the Project's dedicated
// all-member group. The group side must validate the Project pointer before
// writing; ordinary Project-associated groups never use this hook.
type AllMemberGroupAdmitter func(ctx *config.Context, spaceID, groupNo, uid string) error

// AllMemberGroupOwnerTransfer synchronizes the dedicated group's human owner
// with the Project owner. It is idempotent and pointer-scoped.
type AllMemberGroupOwnerTransfer func(ctx *config.Context, projectID, groupNo string) error

// AllMemberGroupRename把全员群名改成 name（D8）。best-effort。
//
// projectID 传的是**发起这次改名的项目**，群侧用它给写加一道归属栅栏：
// 项目侧解析 group_no 的那次读是无锁的，两次之间 P1 的 detach 可以把群变回
// Space 直属，于是这次改名会落在一个已经不属于该项目的群上。见
// renameAllMemberGroup。
type AllMemberGroupRename func(ctx *config.Context, projectID, groupNo, name string) error

var (
	allMemberGroupMu       sync.RWMutex
	allMemberGroupProvish  AllMemberGroupProvisioner
	allMemberGroupAdmitFn  AllMemberGroupAdmitter
	allMemberGroupOwnerFn  AllMemberGroupOwnerTransfer
	allMemberGroupRenameFn AllMemberGroupRename
)

// RegisterAllMemberGroupProvisioner 由 modules/group 在构造时调用。
// 重复注册覆盖（latest wins），方便测试替身。
func RegisterAllMemberGroupProvisioner(fn AllMemberGroupProvisioner) {
	allMemberGroupMu.Lock()
	defer allMemberGroupMu.Unlock()
	allMemberGroupProvish = fn
}

// RegisterAllMemberGroupAdmitter 由 modules/group 在构造时调用。
func RegisterAllMemberGroupAdmitter(fn AllMemberGroupAdmitter) {
	allMemberGroupMu.Lock()
	defer allMemberGroupMu.Unlock()
	allMemberGroupAdmitFn = fn
}

// RegisterAllMemberGroupOwnerTransfer 由 modules/group 在构造时调用。
func RegisterAllMemberGroupOwnerTransfer(fn AllMemberGroupOwnerTransfer) {
	allMemberGroupMu.Lock()
	defer allMemberGroupMu.Unlock()
	allMemberGroupOwnerFn = fn
}

// RegisterAllMemberGroupRename 由 modules/group 在构造时调用。
func RegisterAllMemberGroupRename(fn AllMemberGroupRename) {
	allMemberGroupMu.Lock()
	defer allMemberGroupMu.Unlock()
	allMemberGroupRenameFn = fn
}

func allMemberGroupProvisioner() AllMemberGroupProvisioner {
	allMemberGroupMu.RLock()
	defer allMemberGroupMu.RUnlock()
	return allMemberGroupProvish
}

func allMemberGroupAdmitter() AllMemberGroupAdmitter {
	allMemberGroupMu.RLock()
	defer allMemberGroupMu.RUnlock()
	return allMemberGroupAdmitFn
}

func allMemberGroupOwnerTransfer() AllMemberGroupOwnerTransfer {
	allMemberGroupMu.RLock()
	defer allMemberGroupMu.RUnlock()
	return allMemberGroupOwnerFn
}

func allMemberGroupRename() AllMemberGroupRename {
	allMemberGroupMu.RLock()
	defer allMemberGroupMu.RUnlock()
	return allMemberGroupRenameFn
}

// AllMemberGroupHooksSnapshot holds the registered hooks so a test can put them back.
type AllMemberGroupHooksSnapshot struct {
	provision AllMemberGroupProvisioner
	admit     AllMemberGroupAdmitter
	owner     AllMemberGroupOwnerTransfer
	rename    AllMemberGroupRename
}

// SnapshotAllMemberGroupHooksForTest captures the current registrations.
//
// The registry is process-wide and latest-wins, so a test that installs
// stand-ins disables the REAL hooks for every later case in the same binary.
// That is not hypothetical: modules/project's test binary contains modules/group
// (through the external package's import of internal), so the end-to-end cases
// run against whatever the last stand-in left behind — they passed alone and
// failed in a full run until the stand-ins started restoring.
//
// modules/space's member-removal registry carries the same warning on its own
// substitution, for the same reason.
func SnapshotAllMemberGroupHooksForTest() AllMemberGroupHooksSnapshot {
	allMemberGroupMu.RLock()
	defer allMemberGroupMu.RUnlock()
	return AllMemberGroupHooksSnapshot{
		provision: allMemberGroupProvish,
		admit:     allMemberGroupAdmitFn,
		owner:     allMemberGroupOwnerFn,
		rename:    allMemberGroupRenameFn,
	}
}

// RestoreAllMemberGroupHooksForTest puts a snapshot back.
func RestoreAllMemberGroupHooksForTest(s AllMemberGroupHooksSnapshot) {
	allMemberGroupMu.Lock()
	defer allMemberGroupMu.Unlock()
	allMemberGroupProvish = s.provision
	allMemberGroupAdmitFn = s.admit
	allMemberGroupOwnerFn = s.owner
	allMemberGroupRenameFn = s.rename
}
