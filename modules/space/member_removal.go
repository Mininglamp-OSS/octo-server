package space

import (
	"fmt"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkevent"
	"github.com/Mininglamp-OSS/octo-server/modules/base/event"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

// 成员移除原因。低基数枚举，直接进 DB 与日志，不含用户内容。
const (
	// MemberRemoveReasonKicked owner/admin 通过 members/remove 踢出
	MemberRemoveReasonKicked = "kicked"
	// MemberRemoveReasonLeft 成员自助退出
	MemberRemoveReasonLeft = "left"
	// MemberRemoveReasonForceRemoved 超管在管理端强制移除
	MemberRemoveReasonForceRemoved = "force_removed"
	// MemberRemoveReasonSpaceDisbanded 空间被强制解散，全员一并移除
	MemberRemoveReasonSpaceDisbanded = "space_disbanded"
)

var memberRemoveReasons = map[string]bool{
	MemberRemoveReasonKicked:         true,
	MemberRemoveReasonLeft:           true,
	MemberRemoveReasonForceRemoved:   true,
	MemberRemoveReasonSpaceDisbanded: true,
}

// IsMemberRemoveReason 校验原因取值。写库前拦住拼错的字面量，避免出现
// 永远匹配不到的工单原因。
func IsMemberRemoveReason(reason string) bool { return memberRemoveReasons[reason] }

// MemberRemoval 描述一次已提交的成员移除，传给每个清理步骤。
type MemberRemoval struct {
	SpaceID string
	UID     string
	// OperatorUID 触发这次移除的人；自助退出时等于 UID。
	// 可能并不是目标群/会话的成员，步骤在用它渲染文案时要考虑这一点。
	OperatorUID string
	// Reason 取 MemberRemoveReason* 之一。
	Reason string
}

// MemberRemovalCleanupStep 是一次「把被移除成员从会话面清出去」的可重试步骤。
//
// 契约：
//   - 必须幂等。失败时整条工单会被重跑，已完成的步骤会再执行一次。
//   - 必须自行判断「无事可做」并返回 nil，而不是报错。
//   - 返回 error 表示这次没做完，需要重试；整条工单（含已成功的其它步骤）都会重跑。
type MemberRemovalCleanupStep func(ctx *config.Context, removal MemberRemoval) error

var (
	cleanupStepsMu sync.RWMutex
	cleanupSteps   []namedCleanupStep
)

type namedCleanupStep struct {
	name string
	fn   MemberRemovalCleanupStep
}

// RegisterMemberRemovalCleanupStep 由下游模块在 init 中反向注册清理步骤。
//
// 为什么是反向注册而不是让 space 直接调用 group/user：modules/group 与
// modules/user 都已经 import modules/space，反向 import 即构成 import cycle。
// 同样的手法见 hooks.go 的 DefaultCategoryProvisioner。
//
// 同名重复注册会覆盖（latest wins），方便测试替身。
func RegisterMemberRemovalCleanupStep(name string, fn MemberRemovalCleanupStep) {
	if name == "" || fn == nil {
		return
	}
	cleanupStepsMu.Lock()
	defer cleanupStepsMu.Unlock()
	for i := range cleanupSteps {
		if cleanupSteps[i].name == name {
			cleanupSteps[i].fn = fn
			return
		}
	}
	cleanupSteps = append(cleanupSteps, namedCleanupStep{name: name, fn: fn})
}

// snapshotCleanupSteps 取注册表快照，避免执行期间持锁。
func snapshotCleanupSteps() []namedCleanupStep {
	cleanupStepsMu.RLock()
	defer cleanupStepsMu.RUnlock()
	out := make([]namedCleanupStep, len(cleanupSteps))
	copy(out, cleanupSteps)
	return out
}

// invalidateMemberCaches 移除成员后同步失效两处成员缓存。
//
//   - Redis `space:member:{spaceID}:{uid}`：SpaceMiddleware 的正向缓存，TTL 60s。
//     不清它，被移除的人还能带着这个 space_id 正常访问接口最长 60 秒。这是隔离
//     边界，必须在请求内同步完成。
//   - notify 的进程内成员缓存：仅清本进程那一份，其它副本要等自己的 TTL 到期。
//     这一层只影响卡片/通知的投递目标，不是隔离手段，故接受最终一致。
func (s *Space) invalidateMemberCaches(spaceID, uid string) {
	if spaceID == "" {
		return
	}
	if uid != "" {
		if conn := s.ctx.GetRedisConn(); conn != nil {
			spacepkg.InvalidateMembershipCache(conn, spaceID, uid)
		}
	}
	if event.SpaceMemberCacheInvalidator != nil {
		event.SpaceMemberCacheInvalidator(spaceID)
	}
}

// fireSpaceMemberRemoveEvent 广播 SpaceMemberRemove 观察者事件。
//
// 注意这条事件**不承担**会话面清理的可靠投递——重试由
// space_member_removal_cleanup 工单负责（原因见 event.SpaceMemberRemove 注释）。
// 调用方必须用 `go` 发出：handleEvent 的 listener 分支会在调用者 goroutine 上
// 同步跑完所有监听方，直接调用会把 HTTP handler 阻塞在别的模块的逻辑上。
func (s *Space) fireSpaceMemberRemoveEvent(spaceID, uid, operatorUID, reason string) {
	if s.ctx.Event == nil {
		return
	}
	tx, err := s.ctx.DB().Begin()
	if err != nil {
		s.Error("开启SpaceMemberRemove事件事务失败", zap.Error(err))
		return
	}
	eventID, err := s.ctx.EventBegin(&wkevent.Data{
		Event: event.SpaceMemberRemove,
		Type:  wkevent.Message,
		Data: map[string]interface{}{
			"space_id":     spaceID,
			"uid":          uid,
			"operator_uid": operatorUID,
			"reason":       reason,
		},
	}, tx)
	if err != nil {
		tx.Rollback()
		s.Error("开启SpaceMemberRemove事件失败", zap.Error(err),
			zap.String("spaceId", spaceID), zap.String("uid", uid))
		return
	}
	if err = tx.Commit(); err != nil {
		s.Error("提交SpaceMemberRemove事件事务失败", zap.Error(err))
		return
	}
	s.ctx.EventCommit(eventID)
}

// afterMemberRemoved 成员行提交之后的同步收尾：失效缓存 + 广播事件 + 立刻推一次
// worker。清理工单本身已经在移除事务里写好了（见 enqueueMemberRemovalCleanupTx），
// 这里失败不影响最终一致性，定时调度会兜底。
func (s *Space) afterMemberRemoved(spaceID, uid, operatorUID, reason string) {
	s.invalidateMemberCaches(spaceID, uid)
	go s.fireSpaceMemberRemoveEvent(spaceID, uid, operatorUID, reason)
	go s.processMemberRemovalCleanups()
}

// ---------- worker ----------

// removalWorkerOwner 本进程的租约持有者标识。进程级唯一即可：租约只用来防止
// 同一工单被两个 worker 并发执行，不承担身份语义。
var removalWorkerOwner = "space-removal-" + util.GenerUUID()

// startMemberRemovalCleanupWorker 挂上定时调度。由 Route() 调用，与 user 侧
// processPendingSessionRevocations 的接法一致。
func (s *Space) startMemberRemovalCleanupWorker() {
	s.ctx.Schedule(10*time.Second, s.processMemberRemovalCleanups)
}

// processMemberRemovalCleanups 认领并执行一批清理工单。
//
// 单次最多处理 removalCleanupBatchSize 条；认领失败 / 无可认领工单即返回。
// 并发安全由 DB 侧的 SKIP LOCKED + 租约保证，多副本可同时跑。
func (s *Space) processMemberRemovalCleanups() {
	defer func() {
		if r := recover(); r != nil {
			s.Error("处理成员移除清理工单 panic", zap.Any("recover", r))
		}
	}()
	for processed := 0; processed < removalCleanupBatchSize; processed++ {
		job, err := s.db.claimMemberRemovalCleanup(removalWorkerOwner, time.Now())
		if err != nil {
			s.Error("认领成员移除清理工单失败", zap.Error(err))
			return
		}
		if job == nil {
			return
		}
		s.runMemberRemovalCleanupJob(job)
	}
}

// runMemberRemovalCleanupJob 执行单条工单。
func (s *Space) runMemberRemovalCleanupJob(job *memberRemovalCleanupJob) {
	// 先对齐当前真实成员身份再动手。工单可能在退避期间变陈旧：成员被移除后又重新
	// 加入，这时把他的群和私聊拆掉才是真正的故障。活跃成员 → 工单直接作废。
	member, err := s.db.queryMember(job.SpaceID, job.UID)
	if err != nil {
		s.releaseCleanupJob(job, "membership_recheck_failed", err)
		return
	}
	if member != nil {
		s.Info("被移除成员已重新加入，跳过会话面清理",
			zap.String("spaceId", job.SpaceID), zap.String("uid", job.UID))
		s.finishCleanupJob(job, removalCleanupDone, "skipped_rejoined")
		return
	}

	removal := MemberRemoval{
		SpaceID:     job.SpaceID,
		UID:         job.UID,
		OperatorUID: job.OperatorUID,
		Reason:      job.Reason,
	}
	for _, step := range snapshotCleanupSteps() {
		if err := step.fn(s.ctx, removal); err != nil {
			s.releaseCleanupJob(job, step.name, err)
			return
		}
	}
	s.finishCleanupJob(job, removalCleanupDone, "")
}

// releaseCleanupJob 记一次失败并安排重试；attempts 用尽则置为 abandoned 并高声报错。
func (s *Space) releaseCleanupJob(job *memberRemovalCleanupJob, stepName string, cause error) {
	if job.Attempts+1 >= removalCleanupMaxAttempts {
		s.Error("成员移除清理工单重试耗尽，置为 abandoned",
			zap.Uint64("jobId", job.ID), zap.String("spaceId", job.SpaceID),
			zap.String("uid", job.UID), zap.String("step", stepName),
			zap.Uint32("attempts", job.Attempts+1), zap.Error(cause))
		s.finishCleanupJob(job, removalCleanupAbandoned, stepName+": retries exhausted")
		return
	}
	s.Warn("成员移除清理步骤失败，稍后重试",
		zap.Uint64("jobId", job.ID), zap.String("spaceId", job.SpaceID),
		zap.String("uid", job.UID), zap.String("step", stepName),
		zap.Uint32("attempts", job.Attempts), zap.Error(cause))
	if err := s.db.releaseMemberRemovalCleanup(job.ID, removalWorkerOwner, job.Attempts,
		fmt.Sprintf("%s: %v", stepName, cause)); err != nil {
		s.Warn("释放成员移除清理工单失败", zap.Uint64("jobId", job.ID), zap.Error(err))
	}
}

// finishCleanupJob 写终态。租约易主（affected=0）只记日志：另一个 worker 已接手，
// 重复执行是安全的（步骤契约要求幂等）。
func (s *Space) finishCleanupJob(job *memberRemovalCleanupJob, status uint8, note string) {
	ok, err := s.db.finishMemberRemovalCleanup(job.ID, removalWorkerOwner, status, note)
	if err != nil {
		s.Warn("更新成员移除清理工单终态失败", zap.Uint64("jobId", job.ID), zap.Error(err))
		return
	}
	if !ok {
		s.Warn("成员移除清理工单租约已易主，放弃写入终态", zap.Uint64("jobId", job.ID))
	}
}
