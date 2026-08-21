package space

import (
	"sync"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"go.uber.org/zap"
)

// MemberRejoin 描述一次已提交的成员加入，传给每个恢复步骤。
//
// 与 MemberRemoval 刻意保持同构：两者是同一条边的两个方向，任何一侧新增字段时
// 另一侧多半也要跟上。
type MemberRejoin struct {
	SpaceID string
	UID     string
}

// MemberRejoinRestoreStep 是一次「把加入者重新接回会话面」的步骤。
//
// 契约：
//   - 必须幂等。加入路径有三条（邀请/口令、审批通过、管理端 addMembers），
//     而且都可能对已经是成员的人重复触发。
//   - 必须自行判断「无事可做」并返回 nil。
//   - **不重试。** 这一步是 best-effort：失败只记日志，不阻塞加入流程，也没有工单。
//     取舍见下面 restoreAfterRejoin 的说明。
type MemberRejoinRestoreStep func(ctx *config.Context, rejoin MemberRejoin) error

var (
	rejoinStepsMu sync.RWMutex
	rejoinSteps   []namedRejoinStep
)

type namedRejoinStep struct {
	name string
	fn   MemberRejoinRestoreStep
}

// RegisterMemberRejoinRestoreStep 由下游模块在 init 中反向注册恢复步骤。
// 与 RegisterMemberRemovalCleanupStep 同样是为了避开 import cycle。
// 同名重复注册会覆盖（latest wins），方便测试替身。
func RegisterMemberRejoinRestoreStep(name string, fn MemberRejoinRestoreStep) {
	if name == "" || fn == nil {
		return
	}
	rejoinStepsMu.Lock()
	defer rejoinStepsMu.Unlock()
	for i := range rejoinSteps {
		if rejoinSteps[i].name == name {
			rejoinSteps[i].fn = fn
			return
		}
	}
	rejoinSteps = append(rejoinSteps, namedRejoinStep{name: name, fn: fn})
}

func snapshotRejoinSteps() []namedRejoinStep {
	rejoinStepsMu.RLock()
	defer rejoinStepsMu.RUnlock()
	out := make([]namedRejoinStep, len(rejoinSteps))
	copy(out, rejoinSteps)
	return out
}

// restoreAfterRejoin 跑完所有恢复步骤。**必须在成员行提交之后**调用。
//
// 为什么存在：移除会摘掉 Person 频道白名单（见 modules/user 的 dm_cutoff），
// 而在这次改动之前，没有任何加入路径会把它加回来。WuKongIM 的白名单存储只被
// whitelist_add / whitelist_remove 两个主动 API 改动——注册给它的
// IMDatasource.Whitelist 回调在 CI pin 的版本里根本不会被调用（详见
// modules/group/space_member_removal.go 里那段核实记录）。于是
// 「踢出 → 重新加入」会让这一对永久发不了私聊，而服务端推导侧
// （SharesActiveSpace）已经恢复成 true，annotateDMSendability 还会告诉客户端
// 「可以发」。摘和补必须成对，就像 friend / app_bot / botfather 各自成对那样。
//
// 为什么是 best-effort 而不是工单：
//   - 方向不同，代价不同。漏摘是**越权**（不该能发的还能发），必须重试到成功；
//     漏补是**失能**（该能发的暂时发不了），且用户可见、可自行触发修复
//     （任一方重新加好友、被再次加入、或运维补一次），不构成安全边界破损。
//   - 加入路径是同步 HTTP 请求，为它引一张工单表 + 一个 worker，是把移除侧
//     那套重试机制复制一份来对付一个不对称的问题。
//
// 因此这里只保证「正常情况下摘和补成对」，不保证「IM 故障时最终一定补上」。
// 后者记在 issue #797。
func (s *Space) restoreAfterRejoin(spaceID, uid string) {
	steps := snapshotRejoinSteps()
	if len(steps) == 0 || spaceID == "" || uid == "" {
		return
	}
	rejoin := MemberRejoin{SpaceID: spaceID, UID: uid}
	for _, step := range steps {
		// 逐步 recover：恢复步骤由别的模块注册，一次 panic 不能掀掉加入请求，
		// 也不能让后面的步骤被跳过。
		func() {
			defer func() {
				if r := recover(); r != nil {
					s.Error("成员加入恢复步骤 panic",
						zap.Any("recover", r), zap.String("step", step.name),
						zap.String("spaceId", spaceID), zap.String("uid", uid))
				}
			}()
			if err := step.fn(s.ctx, rejoin); err != nil {
				s.Warn("成员加入恢复步骤失败（best-effort，不阻塞加入）",
					zap.Error(err), zap.String("step", step.name),
					zap.String("spaceId", spaceID), zap.String("uid", uid))
			}
		}()
	}
}
