package space

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
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
	// MemberRemoveReasonBotDeleted Bot 被其所有者删除，账号整体消失，
	// 因此它在**所有** Space 的席位一并关闭（见 CloseAllSpaceSeats）。
	//
	// 它与 force_removed 分开，是因为群侧级联要按 Reason 决定发不发
	// 「X 被 Y 移出群聊」。对一个整体消失的账号，那句话是错的——没有人把它
	// 移出这个群。复用 force_removed 会让这句话出现在它待过的每个群里。
	MemberRemoveReasonBotDeleted = "bot_deleted"
	// MemberRemoveReasonRejoined records a durable 0→1 membership transition.
	// It is consumed by the same cleanup worker as an internal projection
	// intent, but must never execute destructive removal steps.
	MemberRemoveReasonRejoined = "rejoined"
)

var memberRemoveReasons = map[string]bool{
	MemberRemoveReasonKicked:         true,
	MemberRemoveReasonLeft:           true,
	MemberRemoveReasonForceRemoved:   true,
	MemberRemoveReasonSpaceDisbanded: true,
	MemberRemoveReasonBotDeleted:     true,
	MemberRemoveReasonRejoined:       true,
}

// IsMemberRemoveReason 校验原因取值。写库前拦住拼错的字面量，避免出现
// 永远匹配不到的工单原因。
func IsMemberRemoveReason(reason string) bool { return memberRemoveReasons[reason] }

// EnqueueMemberRejoinIntent records a projection-only rejoin responsibility
// using the existing Space cleanup outbox. It is used after a native
// all-member admission has committed but its WuKongIM subscription failed.
// Terminal intents are not reused, so a later stale IMRemove always gets a
// fresh durable retry.
func EnqueueMemberRejoinIntent(
	ctx *config.Context, spaceID, uid, operatorUID string,
) error {
	if ctx == nil {
		return errors.New("space: enqueue rejoin intent requires context")
	}
	tx, err := ctx.DB().Begin()
	if err != nil {
		return fmt.Errorf("space: begin rejoin intent: %w", err)
	}
	defer tx.RollbackUnlessCommitted()
	if err := EnqueueMemberRejoinIntentTx(tx, spaceID, uid, operatorUID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("space: commit rejoin intent: %w", err)
	}
	return nil
}

// MemberRemoval 描述一次已提交的成员移除，传给每个清理步骤。
type MemberRemoval struct {
	SpaceID string
	UID     string
	// OperatorUID 触发这次移除的人；自助退出时等于 UID。
	// 可能并不是目标群/会话的成员，步骤在用它渲染文案时要考虑这一点。
	OperatorUID string
	// Reason 取 MemberRemoveReason* 之一。
	Reason string
	// RejoinCursor is populated only for the internal rejoin projection hook;
	// it is persisted in the existing outbox's last_error column between pages.
	RejoinCursor string
}

const rejoinCursorPrefix = "rejoin_cursor:"

// MemberRejoinPageIncompleteError asks the shared worker to persist the stable
// project cursor before releasing the lease. It is an internal continuation
// signal, not a failed projection.
type MemberRejoinPageIncompleteError struct {
	Cursor string
}

func (e *MemberRejoinPageIncompleteError) Error() string {
	if e == nil || e.Cursor == "" {
		return "rejoin projection page incomplete"
	}
	return "rejoin projection page incomplete after " + e.Cursor
}

func rejoinCursorFromLastError(lastError string) string {
	if len(lastError) <= len(rejoinCursorPrefix) ||
		lastError[:len(rejoinCursorPrefix)] != rejoinCursorPrefix {
		return ""
	}
	cursor := lastError[len(rejoinCursorPrefix):]
	for i, ch := range cursor {
		if ch == '\n' {
			return cursor[:i]
		}
	}
	return cursor
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

// SeatTransition 是一次 space_member 席位状态翻转，交给已注册的事务内步骤。
//
// # 为什么是一个注册表而不是两个
//
// 关席位和开席位对下游是**同一个事实**：「这个人是不是成员」的答案变了。此前这里是
// 两套逐字对称的机件——两个 type、两个 mutex、两个 slice、两个 Register、两个 run——
// 而唯一的注册方 modules/project 给两边注册的是**逐字相同的一行**。
//
// 那份对称不是白拿的，它是本分支两个 P1 的**形状**：
//
//   - 第八轮：移除方向接上了、重新加入方向没接，于是消费方缓存的一条**拒绝**
//     一直和 epoch 对得上，合法回归的成员被持续拒绝，安静的项目里没有上界。
//   - 第九轮：四扇重新激活门只接了两扇，而另外两扇的函数名里根本没有 "reactivate"
//     字样（approveJoinApplyAtomic 甚至是设计好的重新加入漏斗）。
//
// 两次都不是「没想到这个场景」，而是「一个必须手工保持成对的枚举漏了一个元素」。
// 合成一个注册表之后，**「只接了一半」不再可表达**——没有另一半可以漏。
//
// 方向没有丢，它变成了数据（Opened）。需要区分方向的步骤仍然写得出来，而**忘记**
// 区分不再是一个默认行为。
//
// # 与 MemberRemovalCleanupStep 的区别是本质的，不是时机上的微调
//
//   - 清理步骤是**异步**的，可以退避、可以重试、耗尽后进 abandoned 终态。适合
//     「把这个人从各处摘出去」这类最终一致就够的收尾工作。
//   - 事务步骤是**同步**的，它的失败会让整次翻转回滚。只放那些「一旦提交就必须
//     已经成立」的事实——典型的是对外发布的失效信号：如果翻转提交了而信号没发，
//     消费方会拿着一个它自己检查不出过期的判定，而异步补偿的窗口在作业被 abandoned
//     之后是无限的。
type SeatTransition struct {
	// Seat 的 uid 是 `space_member` **存的**那串字节，不是调用方发来的。
	// 见 seatref.go：步骤要拿它去匹配 collation 更严的表。
	Seat SeatRef
	// Opened 为 true 表示一个已关闭的席位被重新打开；false 表示一个活跃席位被关闭。
	Opened bool
}

// SeatTransitionTxStep 是在席位翻转**事务内**同步执行的一步。
//
// 契约：
//   - 必须只做一件小事，并且是**一条语句量级**的。它跑在面向用户的事务里。
//   - 返回 error 会让整次翻转失败。这是刻意的：宁可这次失败让调用方重试，
//     也不要提交一次没有失效信号的翻转。
//   - 必须幂等：调用方可能重试整个事务。
//   - 两个方向都会调用它。如果某一步真的只该对一个方向生效，读 t.Opened——
//     但先确认那不是又一次「只做一半」。
type SeatTransitionTxStep func(tx *dbr.Tx, t SeatTransition) error

var (
	txStepsMu sync.RWMutex
	txSteps   []namedTxStep
)

type namedTxStep struct {
	name string
	fn   SeatTransitionTxStep
}

// RegisterSeatTransitionTxStep 由下游模块在 init 中反向注册事务内步骤。
//
// 反向注册的理由与 RegisterMemberRemovalCleanupStep 相同：modules/project 已经
// import modules/space，反向 import 即构成 import cycle。
//
// 同名重复注册会覆盖（latest wins），方便测试替身。
func RegisterSeatTransitionTxStep(name string, fn SeatTransitionTxStep) {
	if name == "" || fn == nil {
		return
	}
	txStepsMu.Lock()
	defer txStepsMu.Unlock()
	for i := range txSteps {
		if txSteps[i].name == name {
			txSteps[i].fn = fn
			return
		}
	}
	txSteps = append(txSteps, namedTxStep{name: name, fn: fn})
}

// runSeatTransitionTxSteps 在翻转事务内依次执行已注册的同步步骤。
//
// 第一个失败即返回，**不继续执行后续步骤**——与异步清理相反。异步那边步骤之间互不
// 阻塞，因为整条工单会重跑；这边一旦有步骤失败，事务就要回滚，继续跑余下的步骤只是
// 在做注定被丢弃的工作。
//
// 不由业务代码直接调：openSeatTx / closeSeatTx 是唯二的调用点
// （seat_transition.go），这样「翻了席位却忘了发信号」不是一个能写出来的状态。
func runSeatTransitionTxSteps(tx *dbr.Tx, t SeatTransition) error {
	txStepsMu.RLock()
	steps := make([]namedTxStep, len(txSteps))
	copy(steps, txSteps)
	txStepsMu.RUnlock()
	for _, step := range steps {
		if err := step.fn(tx, t); err != nil {
			return fmt.Errorf("space: seat transition tx step %s: %w", step.name, err)
		}
	}
	return nil
}

// snapshotCleanupSteps 取注册表快照，避免执行期间持锁。
func snapshotCleanupSteps() []namedCleanupStep {
	cleanupStepsMu.RLock()
	defer cleanupStepsMu.RUnlock()
	out := make([]namedCleanupStep, len(cleanupSteps))
	copy(out, cleanupSteps)
	return out
}

var (
	rejoinCleanupStepsMu sync.RWMutex
	rejoinCleanupSteps   []namedCleanupStep
)

// RegisterMemberRejoinCleanupStep registers a projection-only convergence hook
// for a durable 0→1 membership intent. Rejoin jobs never invoke ordinary
// removal steps or removal finalizers.
//
// The rejoin registry currently supports one paged projection step. Its cursor
// is persisted on the shared durable job row. Adding another paged step
// requires an explicit independent-cursor protocol; this shared cursor cannot
// represent two independent walks.
func RegisterMemberRejoinCleanupStep(name string, fn MemberRemovalCleanupStep) {
	if name == "" || fn == nil {
		return
	}
	rejoinCleanupStepsMu.Lock()
	defer rejoinCleanupStepsMu.Unlock()
	for i := range rejoinCleanupSteps {
		if rejoinCleanupSteps[i].name == name {
			rejoinCleanupSteps[i].fn = fn
			return
		}
	}
	rejoinCleanupSteps = append(rejoinCleanupSteps, namedCleanupStep{name: name, fn: fn})
}

func snapshotRejoinCleanupSteps() []namedCleanupStep {
	rejoinCleanupStepsMu.RLock()
	defer rejoinCleanupStepsMu.RUnlock()
	out := make([]namedCleanupStep, len(rejoinCleanupSteps))
	copy(out, rejoinCleanupSteps)
	return out
}

var (
	cleanupFinalizersMu sync.RWMutex
	cleanupFinalizers   []namedCleanupStep
)

// RegisterMemberRemovalCleanupFinalizer 注册一个**在全部清理步骤成功之后**才跑的
// 收敛动作。签名与契约与 MemberRemovalCleanupStep 完全相同（幂等、自决、失败即重试），
// 差别只有一条：它读到的是所有步骤都已完成的那个状态。
//
// 为什么需要它，而不是再注册一个步骤：步骤的执行顺序就是注册顺序，而注册顺序由
// import 方向决定，没有任何地方声明过（见 runMemberRemovalCleanupJob 里那段注释）。
// 有些收敛必须看见**别的步骤已经做完**的结果才能算对——例如「全员群的群主必须是
// 项目的活跃 owner」：群侧的级联在离开时会按群资历交接群主，项目侧的级联在关席位，
// 两者谁先谁后不确定，而正确答案只有在两件都发生之后才成立。把这样的动作写成步骤，
// 它在一半的注册顺序下会跑在前面，算出的答案是对另一个中间态的。
//
// 与步骤一样：同名重复注册覆盖（latest wins），方便测试替身。
func RegisterMemberRemovalCleanupFinalizer(name string, fn MemberRemovalCleanupStep) {
	if name == "" || fn == nil {
		return
	}
	cleanupFinalizersMu.Lock()
	defer cleanupFinalizersMu.Unlock()
	for i := range cleanupFinalizers {
		if cleanupFinalizers[i].name == name {
			cleanupFinalizers[i].fn = fn
			return
		}
	}
	cleanupFinalizers = append(cleanupFinalizers, namedCleanupStep{name: name, fn: fn})
}

// snapshotCleanupFinalizers 取注册表快照，避免执行期间持锁。
func snapshotCleanupFinalizers() []namedCleanupStep {
	cleanupFinalizersMu.RLock()
	defer cleanupFinalizersMu.RUnlock()
	out := make([]namedCleanupStep, len(cleanupFinalizers))
	copy(out, cleanupFinalizers)
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
	invalidateSpaceMemberCacheOf(spaceID)
}

// invalidateSpaceMemberCacheOf 是上面那个方法的包级形式，给不持有 *Space 的调用方
// 用（CloseAllSpaceSeats 是包级函数，只拿得到 *config.Context）。一份实现，两个入口。
func invalidateSpaceMemberCacheOf(spaceID string) {
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
	fireSpaceMemberRemoveEventOn(s.ctx, spaceID, uid, operatorUID, reason)
}

// fireSpaceMemberRemoveEventOn 是上面那个方法的包级形式，理由同
// invalidateSpaceMemberCacheOf：CloseAllSpaceSeats 拿不到 *Space。
func fireSpaceMemberRemoveEventOn(ctx *config.Context, spaceID, uid, operatorUID, reason string) {
	logger := log.NewTLog("Space")
	if ctx.Event == nil {
		return
	}
	// 没有任何监听方时不落库。事件行的代价是一次事务 + 后续 QueryWithID 与一条
	// UPDATE（handleEvent 在 listeners == nil 分支上仍会把行标成 Success），
	// 解散一个几千人的空间就是上万次纯浪费的 DB 操作，还会持续撑大 event 表。
	// 保留这条事件是为了给下游留扩展点：一旦有人 AddEventListener，这里自动开始投递。
	// 会话面清理的可靠投递由 space_member_removal_cleanup 工单承担，不依赖本事件。
	if len(ctx.GetEventListeners(event.SpaceMemberRemove)) == 0 {
		return
	}
	tx, err := ctx.DB().Begin()
	if err != nil {
		logger.Error("开启SpaceMemberRemove事件事务失败", zap.Error(err))
		return
	}
	eventID, err := ctx.EventBegin(&wkevent.Data{
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
		logger.Error("开启SpaceMemberRemove事件失败", zap.Error(err),
			zap.String("spaceId", spaceID), zap.String("uid", uid))
		return
	}
	if err = tx.Commit(); err != nil {
		logger.Error("提交SpaceMemberRemove事件事务失败", zap.Error(err))
		return
	}
	ctx.EventCommit(eventID)
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

	removalCleanupAsyncRunning.Add(1)
	go func() {
		defer removalCleanupAsyncRunning.Add(-1)
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
		// 让 CloseAllSpaceSeats 这类拿不到 *Space 的包外调用方也能在入队后
		// 立刻推一轮，而不必干等一个 10s tick。见 member_removal_all_spaces.go。
		setRemovalWorkerKick(func() {
			go func() {
				defer func() {
					if r := recover(); r != nil {
						s.Error("worker kick panic", zap.Any("recover", r))
					}
				}()
				s.processMemberRemovalCleanups()
			}()
		})
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
// 上一轮跑完。一次大解散后队列里堆着成千条工单，一轮 20 条批次可能跑几分钟，
// 期间会叠起几十个并发批次，各自占着 DB 连接猛打 WuKongIM。同一时刻只允许一轮。
var removalCleanupRunning atomic.Bool

// removalCleanupAsyncRunning counts afterMembersRemoved goroutines so tests
// can join prior asynchronous cleanup before reusing the shared test context.
// The counter is observational only; production scheduling remains
// non-blocking.
var removalCleanupAsyncRunning atomic.Int64

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
	if job.Reason == MemberRemoveReasonRejoined {
		// Dispatch asks whether the seat still exists (including a banned Space).
		// Projection hooks separately require active Space/account authorization.
		if !stillMember {
			s.finishCleanupJob(job, owner, removalCleanupDone, "rejoin_not_current")
			return
		}
		canonicalSpaceID, found, err := spacepkg.ResolveSpaceID(s.ctx.DB(), job.SpaceID)
		if err != nil {
			s.releaseCleanupJob(job, owner, "space_identity_resolve_failed", err)
			return
		}
		if !found {
			// A rejoin intent without a Space row has no authoritative projection
			// target. Do not pass a guessed/trimmed identity to recovery hooks.
			s.finishCleanupJob(job, owner, removalCleanupDone, "rejoin_space_not_found")
			return
		}
		removal := MemberRemoval{
			SpaceID:      canonicalSpaceID,
			UID:          job.UID,
			OperatorUID:  job.OperatorUID,
			Reason:       job.Reason,
			RejoinCursor: rejoinCursorFromLastError(job.LastError),
		}
		s.runMemberRejoinCleanupJob(job, owner, removal)
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

	// 收敛动作跑在**全部步骤都成功之后**，这正是它与步骤的唯一区别：它看见的是一个
	// 已经安定的状态，而不是某个注册顺序下的中间态。
	//
	// 有步骤失败就不跑：那一轮里"别的步骤已经做完"这个前提不成立，而工单会被重排，
	// 下一轮全部成功时它自然会跑到。收敛动作自己失败也走同一条重试路径——它和步骤
	// 一样要求幂等，所以整条工单重跑是安全的。
	//
	// panic 与步骤同样在**每一个**收敛动作上单独兜住，理由也一样：函数级的那个
	// recover 会直接跳出循环，让排在后面的收敛动作本轮一次都跑不到。
	for _, fin := range snapshotCleanupFinalizers() {
		err := func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					s.Error("成员移除收敛动作 panic",
						zap.Any("recover", r), zap.Uint64("jobId", job.ID),
						zap.String("finalizer", fin.name),
						zap.String("spaceId", job.SpaceID), zap.String("uid", job.UID))
					err = fmt.Errorf("cleanup finalizer panicked: %v", r)
				}
			}()
			return fin.fn(s.ctx, removal)
		}()
		if err == nil {
			continue
		}
		failedSteps++
		s.Warn("成员移除收敛动作失败，继续执行其余收敛动作",
			zap.Uint64("jobId", job.ID), zap.String("finalizer", fin.name),
			zap.String("spaceId", job.SpaceID), zap.String("uid", job.UID),
			zap.Error(err))
		if firstErr == nil {
			firstFailedStep, firstErr = fin.name, err
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

// runMemberRejoinCleanupJob executes only projection hooks for a rejoin intent.
// It deliberately does not call ordinary removal steps or removal finalizers:
// a 0→1 transition must never close a valid Project seat or remove an
// unrelated group member.
func (s *Space) runMemberRejoinCleanupJob(
	job *memberRemovalCleanupJob, owner string, rejoin MemberRemoval,
) {
	steps := snapshotRejoinCleanupSteps()
	if len(steps) == 0 {
		s.releaseCleanupJob(job, owner, "rejoin_projection_unavailable",
			errors.New("no rejoin projection hook is registered"))
		return
	}
	var (
		firstFailedStep string
		firstErr        error
		failedSteps     int
		resumeCursor    string
		pageIncomplete  int
		pageOnly        = true
	)
	for _, step := range steps {
		err := func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					s.Error("成员重入投影步骤 panic",
						zap.Any("recover", r), zap.Uint64("jobId", job.ID),
						zap.String("step", step.name),
						zap.String("spaceId", job.SpaceID), zap.String("uid", job.UID))
					err = fmt.Errorf("rejoin projection step panicked: %v", r)
				}
			}()
			return step.fn(s.ctx, rejoin)
		}()
		if err == nil {
			continue
		}

		var incomplete *MemberRejoinPageIncompleteError
		if errors.As(err, &incomplete) && incomplete != nil && incomplete.Cursor != "" {
			if resumeCursor == "" {
				resumeCursor = incomplete.Cursor
			} else if resumeCursor != incomplete.Cursor {
				pageOnly = false
				failedSteps++
				if firstErr == nil {
					firstFailedStep = step.name
					firstErr = errors.New("rejoin projection cursors conflict")
				}
			}
			pageIncomplete++
			continue
		}

		pageOnly = false
		failedSteps++
		s.Warn("成员重入投影步骤失败，稍后重试",
			zap.Uint64("jobId", job.ID), zap.String("step", step.name),
			zap.String("spaceId", job.SpaceID), zap.String("uid", job.UID),
			zap.Error(err))
		if firstErr == nil {
			firstFailedStep, firstErr = step.name, err
		}
	}
	if pageOnly && pageIncomplete > 0 && resumeCursor != "" {
		if err := s.db.releaseMemberRejoinCleanup(
			job.ID, owner, job.Attempts, resumeCursor,
		); err != nil {
			s.Warn("释放成员重入投影工单失败",
				zap.Uint64("jobId", job.ID), zap.Error(err))
		}
		return
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
	lastError := fmt.Sprintf("%s: %v", stepName, cause)
	if job.Reason == MemberRemoveReasonRejoined {
		if cursor := rejoinCursorFromLastError(job.LastError); cursor != "" {
			// Retry the failed page from its input cursor, never past a failed
			// project. Keep the failure summary after the continuation header.
			lastError = rejoinCursorPrefix + cursor + "\n" + lastError
		}
	}
	if err := s.db.releaseMemberRemovalCleanup(job.ID, owner, job.Attempts, lastError); err != nil {
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
