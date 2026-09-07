package project

import (
	"sync"

	"github.com/Mininglamp-OSS/octo-lib/config"
)

// 全员群的四个反向注册点（P2）。
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
// 代价被显式接受并各自有兜底：
//
//   - provisioner 失败 → 项目已创建、all_member_group_no 为空串。D4：下一个写路径
//     在租约保护下补建，I4 扫描 A 报出来。
//   - admitter 失败 → 项目席位已提交、人不在全员群里。D12：I4 扫描 B 报出来。
//   - owner / rename 同步失败 → 群主或群名与项目不一致。只记日志加指标；群主那一路
//     还有 P1 的「群主离开项目时移交」兜底。
//
// # 契约
//
// 与 cascade_registry.go 的步骤契约一致，且都必须**幂等**：调用方会在补建和重试
// 路径上重复调用它们。

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

// AllMemberGroupAdmitter 把一个 uid 放进已有的全员群（D12）。
//
// 实现照 modules/group/preset_group_admission.go 的 admitToPresetGroup：自带事务、
// 自取版本号、走唯一准入口、提交后 IM 订阅。幂等——已在群里则整条语句是空操作。
type AllMemberGroupAdmitter func(ctx *config.Context, spaceID, groupNo, uid string) error

// AllMemberGroupOwnerTransfer 确保全员群的群主是这个项目的活跃 owner（D6）。
//
// 签名刻意**不**接受"新群主是谁"。项目可以有多个 owner
// （countActiveOwnersTx 与「最后一个 owner 必须先转让」两处都以此为前提），所以
// "把群主换成刚刚被提升的那个人"是错的：原群主如果本来就是 owner，这次提升与他
// 无关，把群交给新人等于凭空改变了谁控制这个群。
//
// 正确的判定需要"群当前的群主是谁"，而那是群侧的状态。于是这个钩子被写成**自决**
// 的：群侧读出群主，用 pkg/project 问它是不是本项目的活跃 owner，是就什么都不做，
// 不是才移交给一个活跃 owner。这样一来：
//
//   - 幂等，可以在任何一次 owner 变动之后无条件调用；
//   - 多 owner 的情形天然正确；
//   - 项目侧不需要知道群的任何状态，方向仍然是 group → project。
//
// 实现要复用群侧既有的转让原语，而不是自己写一遍 UPDATE：转让要处理旧群主降级、
// 版本号推进、以及客户端的成员变更同步。
//
// 这条路径**绕过** D7 的 handler 层保护，这是有意的：保护挡的是"人手动转让全员群
// 群主"，而这里正是项目侧驱动的那次转让。保护只加在 HTTP handler 上，服务层不加，
// 就是为了让这条路径和 P1 的级联都能通过。
type AllMemberGroupOwnerTransfer func(ctx *config.Context, projectID, groupNo string) error

// AllMemberGroupRename 把全员群名改成 name（D8）。best-effort。
type AllMemberGroupRename func(ctx *config.Context, groupNo, name string) error

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

// AllMemberGroupHooksRegisteredForTest reports whether all four hooks are wired.
//
// Exported for modules/group's construction test. The feature's worst failure
// mode is a SILENT one — an unregistered provisioner means every project is
// created with no group, each occurrence logged and counted but nothing failing
// — so "did construction actually register them" needs to be assertable from the
// module that does the registering.
func AllMemberGroupHooksRegisteredForTest() bool {
	allMemberGroupMu.RLock()
	defer allMemberGroupMu.RUnlock()
	return allMemberGroupProvish != nil && allMemberGroupAdmitFn != nil &&
		allMemberGroupOwnerFn != nil && allMemberGroupRenameFn != nil
}
