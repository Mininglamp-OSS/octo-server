package space

import (
	"errors"
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
	"github.com/gocraft/dbr/v2"
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
//   - 必须幂等。失败时整条工单会被重跑，已成功的步骤会再执行一次。
//   - 必须自行判断「无事可做」并返回 nil，而不是报错。
//   - 返回 error 表示这次没做完，需要重试；整条工单（含已成功的其它步骤）都会重跑。
//   - 步骤之间**互不阻塞**：一个步骤返回 error 不会让同一轮里的其余步骤被跳过
//     （见 runMemberRemovalCleanupJob）。所以不要把「前一个步骤已经成功」当作
//     前置条件——每个步骤都要能独立地从当前 DB 状态判断自己该做什么。
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
//
// 失败必须记日志。这里原先是静默的：DEL 出错时正向条目会活满 TTL，
// SpaceMiddleware 继续放行已被移除的人，而 handler 早已提交并返回 200——
// 一次真实的隔离失效在系统里不留任何痕迹。
//
// 但两种失败要分开报，因为它们的运维含义相反（见 ErrMembershipCacheNegativeFallback）：
// 否定缓存兜底成功时边界当场就生效了，按「可能仍可访问」报出去是**报反了**，而这
// 恰恰是更常见的一种。报反的告警比不报更糟——它把人引向一次并没有发生的越权。
func (s *Space) invalidateMembershipCache(spaceID, uid string) {
	if spaceID == "" || uid == "" {
		return
	}
	conn := s.ctx.GetRedisConn()
	if conn == nil {
		// 没有 Redis 时中间件也读不到缓存，Get 必然未命中并回落到查库，方向是安全的。
		return
	}
	err := spacepkg.InvalidateMembershipCache(conn, spaceID, uid)
	switch {
	case err == nil:
	case errors.Is(err, spacepkg.ErrMembershipCacheNegativeFallback):
		// 正向条目没删掉，但已被否定缓存盖住，中间件当场就拒。边界守住了，
		// 记 Warn 让它可查，不要当越权报。
		s.Warn("清理成员鉴权缓存：DEL 失败，已写否定缓存兜底，隔离仍然生效",
			zap.String("spaceId", spaceID), zap.String("uid", uid), zap.Error(err))
	default:
		s.Error("清理成员鉴权缓存失败：被移除成员可能在缓存 TTL 内仍可访问该 Space",
			zap.String("spaceId", spaceID), zap.String("uid", uid), zap.Error(err))
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

// removalCleanupWorkerOnce 保证整个进程只挂一次定时器。
//
// Route() 在生产里只跑一次，但测试里每建一个 testutil.NewTestServer 就跑一次：
// modules/user 一个包就建 196 个，于是同一个进程里堆起近 400 个永不停止的
// timingwheel 定时器（Schedule 没有取消入口，测试服务器也从不关闭）。它们
// 全都指向同一套 MySQL/Redis/WuKongIM，每 10s 集体醒一次，把 5 分钟的
// per-package 预算耗在与被测用例无关的后台工作上。
//
// 定时器只是「兜底扫描」——真正的即时触发在 afterMembersRemoved 里，工单的
// 跨副本安全由 DB 租约保证，所以挂一次就够；用例需要立即推进时一律直接调
// processMemberRemovalCleanups，不依赖调度。
var removalCleanupWorkerOnce sync.Once

// startMemberRemovalCleanupWorker 挂上定时调度。由 Route() 调用，与 user 侧
// processPendingSessionRevocations 的接法一致。
func (s *Space) startMemberRemovalCleanupWorker() {
	removalCleanupWorkerOnce.Do(func() {
		s.ctx.Schedule(10*time.Second, s.processMemberRemovalCleanups)
		s.ctx.Schedule(time.Hour, s.purgeFinishedMemberRemovalCleanups)
		s.ctx.Schedule(removalSweepInterval, s.sweepExhaustedMemberRemovalCleanups)
		// 指标单独一个更稀疏的节奏：那条查询是全表聚合，而这几个 gauge 是给
		// 分钟级以上的趋势看的，没有必要每分钟扫一次表。
		s.ctx.Schedule(removalMetricsInterval, s.refreshMemberRemovalCleanupMetrics)
	})
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

// removalSweepInterval / removalSweepLimit 控制耗尽工单扫描的节奏与单轮上限。
//
// 一分钟一轮就够：这条扫描处理的是「进程已经被打死」的残留，本来就不是热路径，
// 而单条工单从耗尽到被看见晚一分钟没有任何代价——它已经不会再被认领了。
// 单轮上限防的是一次大范围故障（IM 或 DB 挂过 70 分钟以上）之后，成千条工单同时
// 耗尽预算，一条 UPDATE 就锁住整张表。
const (
	removalSweepInterval = time.Minute
	removalSweepLimit    = 500
	// removalMetricsInterval 指标采集节奏。比扫描稀疏，因为那条查询是全表聚合
	// （MIN(created_at) 无索引可用），而 gauge 服务的是趋势判断，不是秒级响应。
	removalMetricsInterval = 5 * time.Minute
)

// sweepExhaustedMemberRemovalCleanups 把重试预算耗尽、租约也已过期的工单推到 abandoned。
//
// 日志级别是 Error 而不是 Warn，而且刻意每轮都打：abandoned 没有任何自动重驱动，
// 被移除的人会一直留在该 Space 的群里和 IM 群订阅里，直到有人介入。这是本条唯一
// 的出口信号，在 /metrics 端点落地之前它就是告警面。
func (s *Space) sweepExhaustedMemberRemovalCleanups() {
	abandoned, err := s.db.abandonExhaustedMemberRemovalCleanups(time.Now().UTC(), removalSweepLimit)
	if err != nil {
		s.Warn("扫描重试耗尽的成员移除工单失败", zap.Error(err))
		return
	}
	if abandoned > 0 {
		s.Error("成员移除清理工单重试耗尽，已置为 abandoned；无自动重驱动，需人工介入",
			zap.Int64("abandoned", abandoned))
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
// panic 必须在**这一层**兜住：清理步骤是别的模块注册进来的，一次 panic 若只被
// 批次层的 recover 接住，就会绕过 releaseCleanupJob —— 工单停在 running 上，
// 既没有 last_error 也没有退避，只能干等 removalCleanupLease（10 分钟）到期才
// 重新可认领；然后被再次认领、再次 panic，如此循环。attempts 虽然在认领时就已
// 自增（见 claimMemberRemovalCleanup），所以最终仍会走到 abandoned，但每一轮都
// 要白烧一个租约周期；而每一次重新认领又白占一个批次名额，把同批本该被处理的
// 健康工单挤出去。
// 在这一层 recover 并显式 release，才能立刻记下原因、按退避重排。
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
	// 加入，这时把他的群拆掉才是真正的故障。仍持有席位 → 工单直接作废。
	//
	// 谓词必须与级联步骤里那道门完全一致（CheckMembershipForCleanup），否则外层门
	// 先跑、先短路，内层那个谓词根本没机会执行。这里以前用 queryMember，只看
	// space_member.status=1、不问 Space 死没死：join-vs-disband 竞态造出的孤儿行
	//（Space 已 status=0，成员行被并发 join 写回 status=1）会被误判成「人已重新
	// 加入」，工单当场作废，那个人的 group_member 行和 IM 群订阅就永远留在一个
	// 已解散的空间里，再没有任何东西会回来看一眼。
	stillMember, err := spacepkg.CheckMembershipForCleanup(s.ctx.DB(), job.SpaceID, job.UID)
	if err != nil {
		s.releaseCleanupJob(job, owner, "membership_recheck_failed", err)
		return
	}
	if stillMember {
		s.Info("被移除成员仍持有 Space 席位，跳过会话面清理",
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
	// 一个步骤失败**不中断**其余步骤。
	//
	// 今天只注册了 group_cascade 一个步骤，所以这一层是**防御性**的：它保护的是
	// 注册表这个扩展点，而不是当前的某个具体失败。
	//
	// 之所以一开始就这么写，是因为 fail-fast 的版本在两个步骤并存时真的出过问题：
	// 步骤顺序就是注册顺序，而注册顺序由 import 方向决定，没有任何地方声明过。
	// 排在前面的那个步骤一旦持续失败（例如 WuKongIM 故障），20 次尝试
	// （约 70 分钟退避）会全部烧在它身上，工单走到 abandoned 时后面的步骤
	// **一次都没跑过**。那正是这条链路要消灭的隔离失败，却发生在最需要它生效的场景里。
	// 后续 PR 把私聊清理步骤加回来时，这个性质必须仍然成立。
	//
	// 步骤契约本来就要求幂等（见 MemberRemovalCleanupStep），所以「已经成功的步骤
	// 在重试时再跑一遍」是允许的，那也正是 fail-fast 唯一换来的东西——它并没有
	// 保护任何不变量，只是让排在前面的步骤独占了整个预算。
	//
	// 仍然只保留首个错误上报：last_error 是 VARCHAR(255) 的低基数摘要，
	// 把 N 个步骤的错误拼进去只会互相截断；每个失败步骤各自有自己的日志行。
	var (
		firstFailedStep string
		firstErr        error
		failedSteps     int
	)
	for _, step := range snapshotCleanupSteps() {
		// panic 也必须**在这一层**兜住，不能只靠函数级的那个 recover。
		//
		// 上面那个 defer 在整个函数的作用域上，一次 panic 会直接跳出这个循环——
		// 于是一个步骤 panic 时，排在它后面的步骤本轮一次都不会跑，而 attempts 在
		// 认领时就已自增，工单照样一路走到 abandoned。那正是上面那段注释说要消灭的
		// 失败，只是换成 panic 这条路径进来。同样是为多步骤准备的防御。
		err := func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					s.Error("成员移除清理步骤 panic",
						zap.Any("recover", r), zap.Uint64("jobId", job.ID),
						zap.String("step", step.name),
						zap.String("spaceId", job.SpaceID), zap.String("uid", job.UID))
					err = fmt.Errorf("cleanup step panicked: %v", r)
				}
			}()
			return step.fn(s.ctx, removal)
		}()
		if err == nil {
			continue
		}
		failedSteps++
		s.Warn("成员移除清理步骤失败，继续执行其余步骤",
			zap.Uint64("jobId", job.ID), zap.String("step", step.name),
			zap.String("spaceId", job.SpaceID), zap.String("uid", job.UID),
			zap.Error(err))
		if firstErr == nil {
			firstFailedStep, firstErr = step.name, err
		}
	}
	if firstErr != nil {
		if failedSteps > 1 {
			firstFailedStep = fmt.Sprintf("%s(+%d)", firstFailedStep, failedSteps-1)
		}
		s.releaseCleanupJob(job, owner, firstFailedStep, firstErr)
		return
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

// HasPendingRemovalCleanup 报告 (spaceID, uid) 是否还有未完成的移除清理工单。
//
// 给 group 侧的群主交接通告用：批量移除按 uid 逐条建工单，若被移除的几个人正好是同一个
// 群里连续的元老，交接会沿元老顺序连锁 C→S2、S2→S3……每一环都想发一条「已成为新群主」，
// 而前面那些在写下时就已作废。动手通告前先问一句「这位继任者自己是不是也在待移除队列
// 里」，是就不发，链条于是只剩最后一环——那一环的继任者不在队列里，通告的是最终结果。
//
// ⚠️ 这只在同批工单于任何 worker 起跑前就全部可见时才成立，而**并非所有入口都如此**：
// 解散走 enqueueMemberRemovalCleanupBatchTx、超管强制移除走 removeMembersForce，两者都是
// 单事务原子入队；但用户端 members/remove 走 removeMemberLocked，一人一事务逐个提交
// （reason=kicked，不抑制），后面几个 uid 的行在前缀被认领时还不存在，本函数会把他们
// 读成「不在队列里」。后果与完整分析见调用方 group/space_member_removal.go 里
// HasPendingRemovalCleanup 调用点上方的注释。
//
// 只看 pending（status=0）：
//   - done 表示那条工单已跑完，人已经不在群里，本来也不会被选为继任者；
//   - abandoned 表示重试耗尽、放弃了，这个人不会再被移除，所以该照常通告。
//
// ⚠️ 上面那条 abandoned 只在**检查发生时它已经是 abandoned** 才成立。真实次序通常
// 相反：检查跑在前，继任者的工单之后才耗尽重试（20 次约 70 分钟）走到 abandoned。
// 那种次序下这里读到的是 pending → 抑制 → 而那个人最终留下来当了群主，**再没有任何
// 东西会补发通告**，群里又回到「凭空多出新群主」。继任者中途重新加入
// （工单被标 skipped_rejoined）是同一类：检查那一刻仍是 pending。
// 根治要在一批工单全部终结后重新评估，而不是逐环当场决定——与 #797 里那些
// 「副作用需要持久化重放」的条目是同一个问题，归在那里。
//
// 已知的保守失败方向：继任者若挂着一条更早的、卡住不动的 pending 工单，这里会误判成
// 「他也要走」而少发一条通告。相比反过来（通告一个马上就作废的群主）这个方向更可接受，
// 记在 brief 的 out-of-scope 里。
func HasPendingRemovalCleanup(session *dbr.Session, spaceID, uid string) (bool, error) {
	if spaceID == "" || uid == "" {
		return false, nil
	}
	var count int
	err := session.SelectBySql(
		"SELECT COUNT(*) FROM space_member_removal_cleanup WHERE space_id=? AND uid=? AND status=?",
		spaceID, uid, removalCleanupPending,
	).LoadOne(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}
