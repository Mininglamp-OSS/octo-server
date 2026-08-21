package space

import (
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
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

// invalidateMembershipCache 清掉某个成员在某个 Space 的 SpaceMiddleware 正向缓存。
//
// Redis key `space:member:{spaceID}:{uid}`，TTL 60s。不清它，被移除的人还能带着
// 这个 space_id 正常访问接口最长 60 秒。这是隔离边界，必须在请求内同步完成，
// 不能丢给后台。
func (s *Space) invalidateMembershipCache(spaceID, uid string) {
	if spaceID == "" || uid == "" {
		return
	}
	if conn := s.ctx.GetRedisConn(); conn != nil {
		spacepkg.InvalidateMembershipCache(conn, spaceID, uid)
	}
}

// invalidateSpaceMemberCache 清掉 notify 的进程内成员缓存。
//
// 粒度是整个 Space，所以批量移除时调一次就够，不必逐个成员重复调。
// 只清本进程那一份，其它副本要等自己的 TTL（60s）到期；这一层只影响卡片/通知的
// 投递目标，不是隔离手段，故接受最终一致。
func (s *Space) invalidateSpaceMemberCache(spaceID string) {
	if spaceID == "" {
		return
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
	// 没有任何监听方时不落库。事件行的代价是一次事务 + 后续 QueryWithID 与一条
	// UPDATE（handleEvent 在 listeners == nil 分支上仍会把行标成 Success），
	// 解散一个几千人的空间就是上万次纯浪费的 DB 操作，还会持续撑大 event 表。
	// 保留这条事件是为了给下游留扩展点：一旦有人 AddEventListener，这里自动开始投递。
	// 会话面清理的可靠投递由 space_member_removal_cleanup 工单承担，不依赖本事件。
	if len(s.ctx.GetEventListeners(event.SpaceMemberRemove)) == 0 {
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

// afterMembersRemoved 成员行提交之后的收尾。清理工单本身已经在移除事务里写好了
// （见 enqueueMemberRemovalCleanupTx），这里失败不影响最终一致性，定时调度会兜底。
//
// 分成两段是有意的：
//   - 鉴权缓存逐个**同步**清掉。这是隔离边界，必须在 HTTP 响应之前完成，
//     否则被移除的人还有最长 60s 的访问窗口。
//   - 事件广播与 worker 触发放进**单个**后台 goroutine 串行做。早先的写法是每个
//     uid 各起一个 goroutine，解散一个几千人的 Space 就会瞬间起几千个 goroutine，
//     每个都开事务写事件、抢清理工单——把一次管理操作变成一场自我 DDoS。
func (s *Space) afterMembersRemoved(spaceID string, uids []string, operatorUID, reason string) {
	if spaceID == "" || len(uids) == 0 {
		return
	}
	for _, uid := range uids {
		s.invalidateMembershipCache(spaceID, uid)
	}
	s.invalidateSpaceMemberCache(spaceID)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.Error("成员移除收尾 panic", zap.Any("recover", r), zap.String("spaceId", spaceID))
			}
		}()
		for _, uid := range uids {
			s.fireSpaceMemberRemoveEvent(spaceID, uid, operatorUID, reason)
		}
		s.processMemberRemovalCleanups()
	}()
}

// afterMemberRemoved 单成员收尾，afterMembersRemoved 的便捷包装。
func (s *Space) afterMemberRemoved(spaceID, uid, operatorUID, reason string) {
	s.afterMembersRemoved(spaceID, []string{uid}, operatorUID, reason)
}

// ---------- worker ----------

// removalWorkerPrefix 本进程标识，只用于让日志里能看出是哪个副本在跑。
// 刻意保持短：租约标识要连同下面的计数器一起塞进 lease_owner VARCHAR(64)。
var removalWorkerPrefix = "sr-" + util.GenerUUID()

// removalClaimSeq 进程内单调计数器，给每次认领配一个唯一后缀。
// 用计数器而不是再拼一个 UUID：两个 32 位 UUID 加前缀是 78 字符，超过
// lease_owner 的列宽，MySQL 会直接以 "Data too long" 拒掉整条认领。
var removalClaimSeq atomic.Uint64

// newRemovalClaimOwner 为**每一次认领**生成唯一的租约持有者标识。
//
// 不能用进程级常量：afterMembersRemoved 起的那个 goroutine 与 10s 定时器会同时
// 调 processMemberRemovalCleanups。群级联要逐个群调 RemoveGroupMembers（每个群都
// 有 IM 退订 + 发 Tip + 子区清理），大空间下仍可能跑满 removalCleanupLease；一过期，另一个
// goroutine 就能重新认领同一条工单。若两者 owner 相同，finish/release 上的
// `AND lease_owner=?` 对双方都成立——先跑完的把工单标成终态，另一个还在半路，
// 群里于是出现重复的「被移出」系统消息，慢的那个再 release 还会把已完成的工单
// 复活。每次认领一个新 owner，就能让晚到的那个写入落空并被察觉。
func newRemovalClaimOwner() string {
	return removalWorkerPrefix + "-" + strconv.FormatUint(removalClaimSeq.Add(1), 10)
}

// startMemberRemovalCleanupWorker 挂上定时调度。由 Route() 调用，与 user 侧
// processPendingSessionRevocations 的接法一致。
func (s *Space) startMemberRemovalCleanupWorker() {
	s.ctx.Schedule(10*time.Second, s.processMemberRemovalCleanups)
	s.ctx.Schedule(time.Hour, s.purgeFinishedMemberRemovalCleanups)
}

// purgeFinishedMemberRemovalCleanups 定期清掉超过保留期的终态工单。
// 工单只翻状态不删除，不清理的话这张表会随每一次踢人 / 退出 / 解散无限增长，
// 把每 10s 一次的 pending 扫描越拖越慢。
func (s *Space) purgeFinishedMemberRemovalCleanups() {
	const purgeLimit = 1000
	deleted, err := s.db.purgeFinishedMemberRemovalCleanups(
		time.Now().UTC().Add(-removalCleanupRetention), purgeLimit)
	if err != nil {
		s.Warn("清理过期成员移除工单失败", zap.Error(err))
		return
	}
	if deleted > 0 {
		s.Info("清理过期成员移除工单", zap.Int64("deleted", deleted))
	}
}

// removalCleanupRunning 进程内重入保护。
//
// 触发源有两个：afterMembersRemoved 起的 goroutine，和每 10s 一次的定时器；而定时器
// 是「先安排下一次、再执行本次」（timingwheel 每次 firing 都 `go task()`），并不会等
// 上一轮跑完。一次大解散后队列里堆着成千条工单，一轮 20 条的批次可能跑几分钟，
// 期间会叠起几十个并发批次，各自占着 DB 连接猛打 WuKongIM。同一时刻只允许一轮。
var removalCleanupRunning atomic.Bool

// processMemberRemovalCleanups 认领并执行一批清理工单。
//
// 单次最多处理 removalCleanupBatchSize 条；认领失败 / 无可认领工单即返回。
// 跨副本的并发安全由 DB 侧的 SKIP LOCKED + 租约保证；进程内由上面的 running 标志
// 保证只有一轮在跑。
func (s *Space) processMemberRemovalCleanups() {
	if !removalCleanupRunning.CompareAndSwap(false, true) {
		return // 已有一轮在跑，本次直接让位
	}
	defer removalCleanupRunning.Store(false)
	defer func() {
		if r := recover(); r != nil {
			s.Error("处理成员移除清理工单 panic", zap.Any("recover", r))
		}
	}()
	for processed := 0; processed < removalCleanupBatchSize; processed++ {
		owner := newRemovalClaimOwner()
		job, err := s.db.claimMemberRemovalCleanup(owner, time.Now().UTC())
		if err != nil {
			s.Error("认领成员移除清理工单失败", zap.Error(err))
			return
		}
		if job == nil {
			return
		}
		s.runMemberRemovalCleanupJob(job, owner)
	}
}

// runMemberRemovalCleanupJob 执行单条工单。
//
// panic 必须在**这一层**兜住：清理步骤是别的模块注册进来的，一次 panic 若穿到
// 批次层，就会绕过 releaseCleanupJob —— attempts 不增、状态还是 pending、租约
// 60s 后自然到期，于是同一条工单被反复认领、反复 panic，永远到不了 abandoned；
// 而且它按 `ORDER BY id` 一直排在队首，把后面所有工单都饿死。
func (s *Space) runMemberRemovalCleanupJob(job *memberRemovalCleanupJob, owner string) {
	defer func() {
		if r := recover(); r != nil {
			s.Error("成员移除清理步骤 panic",
				zap.Any("recover", r), zap.Uint64("jobId", job.ID),
				zap.String("spaceId", job.SpaceID), zap.String("uid", job.UID))
			s.releaseCleanupJob(job, owner, "panic", fmt.Errorf("cleanup step panicked: %v", r))
		}
	}()
	// 先对齐当前真实成员身份再动手。工单可能在退避期间变陈旧：成员被移除后又重新
	// 加入，这时把他的群和私聊拆掉才是真正的故障。活跃成员 → 工单直接作废。
	member, err := s.db.queryMember(job.SpaceID, job.UID)
	if err != nil {
		s.releaseCleanupJob(job, owner, "membership_recheck_failed", err)
		return
	}
	if member != nil {
		s.Info("被移除成员已重新加入，跳过会话面清理",
			zap.String("spaceId", job.SpaceID), zap.String("uid", job.UID))
		s.finishCleanupJob(job, owner, removalCleanupDone, "skipped_rejoined")
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
			s.releaseCleanupJob(job, owner, step.name, err)
			return
		}
	}
	s.finishCleanupJob(job, owner, removalCleanupDone, "")
}

// releaseCleanupJob 记一次失败并安排重试；attempts 用尽则置为 abandoned 并高声报错。
func (s *Space) releaseCleanupJob(job *memberRemovalCleanupJob, owner, stepName string, cause error) {
	if job.Attempts >= removalCleanupMaxAttempts {
		s.Error("成员移除清理工单重试耗尽，置为 abandoned",
			zap.Uint64("jobId", job.ID), zap.String("spaceId", job.SpaceID),
			zap.String("uid", job.UID), zap.String("step", stepName),
			zap.Uint32("attempts", job.Attempts), zap.Error(cause))
		s.finishCleanupJob(job, owner, removalCleanupAbandoned, stepName+": retries exhausted")
		return
	}
	s.Warn("成员移除清理步骤失败，稍后重试",
		zap.Uint64("jobId", job.ID), zap.String("spaceId", job.SpaceID),
		zap.String("uid", job.UID), zap.String("step", stepName),
		zap.Uint32("attempts", job.Attempts), zap.Error(cause))
	if err := s.db.releaseMemberRemovalCleanup(job.ID, owner, job.Attempts,
		fmt.Sprintf("%s: %v", stepName, cause)); err != nil {
		s.Warn("释放成员移除清理工单失败", zap.Uint64("jobId", job.ID), zap.Error(err))
	}
}

// finishCleanupJob 写终态。租约易主（affected=0）只记日志：另一个 worker 已接手，
// 重复执行是安全的（步骤契约要求幂等）。
func (s *Space) finishCleanupJob(job *memberRemovalCleanupJob, owner string, status uint8, note string) {
	ok, err := s.db.finishMemberRemovalCleanup(job.ID, owner, status, note)
	if err != nil {
		s.Warn("更新成员移除清理工单终态失败", zap.Uint64("jobId", job.ID), zap.Error(err))
		return
	}
	if !ok {
		s.Warn("成员移除清理工单租约已易主，放弃写入终态", zap.Uint64("jobId", job.ID))
	}
}
