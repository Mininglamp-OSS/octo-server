package conversation_ext

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/gocraft/dbr/v2"
)

// target_type constants — kept package-private; callers use the Service API.
const (
	targetTypeDM     uint8 = 1
	targetTypeGroup  uint8 = 2
	targetTypeThread uint8 = 5
)

// threadSeparator is the fixed four-underscore delimiter used in thread
// channel IDs: "{groupNo}____{shortID}".
const threadSeparator = "____"

// ErrThreadForbidden 在 FollowThread 鉴权失败时返回。
// 调用方（HTTP handler）应将此错误翻译为 403。
var ErrThreadForbidden = errors.New("thread follow forbidden: not a member of parent group or thread not visible")

// ErrChannelForbidden 在 FollowChannel 鉴权失败时返回。
// 调用方（HTTP handler）应将此错误翻译为 403。
//
// 引入背景（PR #123 round-1 review by Jerry-Xin / yujiawei）：FollowChannel
// 不再只是 inert 的"清自己的黑名单"，而是会触发 thread ext fanout 订阅 +
// 物化既有子区，因此必须在写前校验 caller 是该 group 的成员且该群在请求 Space 可见。
var ErrChannelForbidden = errors.New("channel follow forbidden: not a member of the group or group not visible")

// ErrDMCategoryForbidden 在 FollowDM 指定的 category 不属于当前 uid 或已删除时返回。
// 调用方应将此错误翻译为 400 / 403（按业务约定）。
// PR #21 Round-6 (Jerry-Xin)：DM category 必须由服务端校验归属，否则客户端可写入
// 任意 UUID 让自己的 follow tab 引用不存在的分类（"未分类"渲染）。
var ErrDMCategoryForbidden = errors.New("dm category forbidden: not owned by uid or category deleted")

// ThreadAuthChecker 判定 FollowThread 是否被授权，是一个 narrow interface。
// 为避免对 modules/group / modules/thread 形成循环依赖，采用依赖倒置从外部注入。
//
// AuthorizeThreadFollow 一次性校验：
//   - shortID 对应的 thread 存在，且 status != deleted。
//   - thread.group_no == 入参 groupNo（拒绝跨群引用）。
//   - uid 是 groupNo 的成员。
//
// 鉴权失败返回 ErrThreadForbidden（具体原因由 handler 写日志）。
// 校验通过返回 nil。基础设施错误（DB 错误等）以 wrap 后的形式向上透传。
type ThreadAuthChecker interface {
	AuthorizeThreadFollow(uid, spaceID, groupNo, shortID string) error
}

// ChannelAuthChecker 判定 FollowChannel 是否被授权。窄接口，与 ThreadAuthChecker
// 同样采用依赖倒置（避免 conversation_ext 直接 import group），由 message/1module.go
// 注入实现：调用 group.IService.ExistMember + group.DB 可见性逻辑。
//
// 鉴权失败返回 ErrChannelForbidden；基础设施错误以 wrap 后形式上传。
type ChannelAuthChecker interface {
	AuthorizeChannelFollow(uid, spaceID, groupNo string) error
}

// ThreadEnumerator 是 FollowChannel 级联物化子区时使用的窄接口。
// 与 ThreadAuthChecker 一样采用依赖倒置，避免 conversation_ext 直接 import thread。
// 由 message/1module.go 启动时通过 SetThreadEnumerator 注入；nil 时跳过物化
// （供单测以及尚未注入的迁移期使用）。
//
// EnumerateActiveShortIDs 返回 groupNo 下 status=active 的子区 shortID 列表，
// 最多返回 limit 项。返回顺序由调用方约定（通常按 created_at DESC），
// 与 maxAutoFollowThreadsPerChannel 的截断语义一致。
type ThreadEnumerator interface {
	EnumerateActiveShortIDs(groupNo string, limit int) ([]string, error)
}

// maxAutoFollowThreadsPerChannel 是 FollowChannel 一次性物化的子区数量上限。
// 与 maxUpdateSortItems=500 同审美：覆盖产品上限场景同时把 tx 锁范围与延迟控制住。
// 超过该数量的子区不在 FollowChannel 时物化，依赖产品侧"子区自动归档"把活跃数控制在 cap 内
// + 后续 OnThreadCreated fanout 持续补齐。
const maxAutoFollowThreadsPerChannel = 500

// onThreadCreatedBatchSize 是 OnThreadCreated 给 follower 做 fanout 时单个 SQL
// 处理的最大 (uid, space_id) 数量。MySQL prepared statement 占位符上限是
// 65,535，本 fanout 单行 bulk INSERT IGNORE 用 4 个占位符 / bulk version bump
// 用 3 个占位符。1000 留出充分余量同时让单 tx 锁窗口可控：N=10k follower 时
// 按 10 个 tx 跑，每 tx 仅持锁 ~20ms 数量级。
// var 而非 const 是为了让测试压到一个小值以低成本覆盖分批分支。
var onThreadCreatedBatchSize = 1000

// Service encapsulates composite operations on user_conversation_ext that
// require a single transaction boundary.  It intentionally avoids importing
// modules/group, modules/user, or modules/thread to prevent circular
// dependencies.
//
// threadAuth 是 FollowThread 的鉴权钩子，由外部模块（在 1module.go 里把
// group/thread 组合起来的实现）在启动时通过 SetThreadAuthChecker 注入。
// 为 nil 时跳过鉴权（仅供测试 / 迁移期使用）。
//
// （历史 DMCategoryChecker 注入点 issue #75 / PR #79 fix 之后已移除——FollowDM
// 鉴权改为事务内 SELECT ... FOR UPDATE，见 authorizeDMCategoryInTx——曾经的
// `dmCatAuth`/`SetDMCategoryChecker` 接口与对应的 message 模块注入也一起清掉。）
type Service struct {
	db          *DB
	session     *dbr.Session
	threadAuth  ThreadAuthChecker
	threadAuthM sync.RWMutex
	// threadEnum 是 FollowChannel 级联物化子区时的查询钩子。
	// 由 message/1module.go 启动时注入；nil 时跳过物化。
	threadEnum  ThreadEnumerator
	threadEnumM sync.RWMutex
	// channelAuth 是 FollowChannel 的群成员/可见性鉴权钩子。
	// 由 message/1module.go 启动时注入；nil 时跳过鉴权（仅供单测 / 迁移期使用）。
	channelAuth  ChannelAuthChecker
	channelAuthM sync.RWMutex
	log.Log
}

// NewService creates a Service.
func NewService(ctx *config.Context) *Service {
	return &Service{
		db:      NewDB(ctx),
		session: ctx.DB(),
		Log:     log.NewTLog("ConvExtService"),
	}
}

// SetThreadAuthChecker injects the auth checker used by FollowThread.
// Safe for concurrent use; intended to be called once at startup from
// 1module.go after the group / thread modules have initialised.
func (s *Service) SetThreadAuthChecker(c ThreadAuthChecker) {
	s.threadAuthM.Lock()
	s.threadAuth = c
	s.threadAuthM.Unlock()
}

// getThreadAuthChecker returns the currently registered checker (or nil).
func (s *Service) getThreadAuthChecker() ThreadAuthChecker {
	s.threadAuthM.RLock()
	c := s.threadAuth
	s.threadAuthM.RUnlock()
	return c
}

// SetThreadEnumerator injects the enumerator used by FollowChannel to
// materialize thread ext rows for every active thread under the channel.
// Safe for concurrent use; intended to be called once at startup from
// message/1module.go after the thread module has initialised.
func (s *Service) SetThreadEnumerator(e ThreadEnumerator) {
	s.threadEnumM.Lock()
	s.threadEnum = e
	s.threadEnumM.Unlock()
}

func (s *Service) getThreadEnumerator() ThreadEnumerator {
	s.threadEnumM.RLock()
	e := s.threadEnum
	s.threadEnumM.RUnlock()
	return e
}

// SetChannelAuthChecker injects the authorizer used by FollowChannel.
// Safe for concurrent use; intended to be called once at startup from
// message/1module.go after the group module has initialised.
func (s *Service) SetChannelAuthChecker(c ChannelAuthChecker) {
	s.channelAuthM.Lock()
	s.channelAuth = c
	s.channelAuthM.Unlock()
}

func (s *Service) getChannelAuthChecker() ChannelAuthChecker {
	s.channelAuthM.RLock()
	c := s.channelAuth
	s.channelAuthM.RUnlock()
	return c
}

// ---------------------------------------------------------------------------
// Input validation helpers
// ---------------------------------------------------------------------------

func validateBase(uid, spaceID string) error {
	if uid == "" {
		return errors.New("uid must not be empty")
	}
	if spaceID == "" {
		return errors.New("space_id must not be empty")
	}
	return nil
}

// parseThreadChannelID splits a thread channel ID of the form
// "{groupNo}____{shortID}" and returns groupNo, shortID.
// Returns an error if the format is invalid.
func parseThreadChannelID(threadChannelID string) (groupNo, shortID string, err error) {
	parts := strings.SplitN(threadChannelID, threadSeparator, 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("thread_channel_id %q is invalid: expected format {groupNo}____{shortID}", threadChannelID)
	}
	return parts[0], parts[1], nil
}

// threadLikePrefix 构造 "{groupNo}____%" 的 LIKE 前缀，并用 '|' 作为转义符
// （配合 ESCAPE '|' 使用）。集中在一处避免不同调用方对 ESCAPE 字符产生分歧。
func threadLikePrefix(groupNo string) string {
	return escapeLike(groupNo) + escapeLike(threadSeparator) + "%"
}

// escapeLike escapes LIKE special characters for use with ESCAPE '|'.
// The pipe character is chosen as the escape character because it never
// appears in snowflake IDs or our thread channel IDs, avoiding the
// double-backslash quoting problem when passing '\' through the Go MySQL driver.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `|`, `||`)
	s = strings.ReplaceAll(s, `%`, `|%`)
	s = strings.ReplaceAll(s, `_`, `|_`)
	return s
}

// ---------------------------------------------------------------------------
// FollowChannel — clear group-blacklist flag (re-follow a previously unfollowed group)
// ---------------------------------------------------------------------------

// FollowChannel marks the group as followed (group_unfollowed=0,
// auto_follow_threads=1) and materializes thread ext rows for up to
// maxAutoFollowThreadsPerChannel currently-active threads under the channel.
//
// 两阶段提交（bug fix #2 race window）：
//
//  1. Phase 1 (tx)  ：bump follow_version + upsert 群行 (group_unfollowed=0,
//     auto_follow_threads=1)，commit。auto_follow=1 一旦可见，并发新建的子区在
//     thread.Service post-commit hook 中触发的 OnThreadCreated 会把本用户当作
//     fanout 目标 ——── 这条 invariant 是覆盖 race window 的核心。
//
//  2. Phase 2 (无 tx)：enumerate 当前 active 子区。在 Phase 1 commit 之后
//     做这一步，意味着任意在 Phase 1 commit 之前已存在的子区都会进入快照；
//     任意在快照之后才创建的子区则由 Phase 1 commit 后的 OnThreadCreated 兜底。
//     两条路径合起来无遗漏；INSERT IGNORE 让任何重叠（同子区被两条路径都写）
//     安全降为 no-op。
//
//  3. Phase 3 (tx)  ：bump follow_version + bulk INSERT IGNORE thread ext 行。
//     失败时记日志但保留 Phase 1 的写入 —— 客户端会感知到 version bump 而触发
//     重拉；missing 子区在下次 FollowChannel 或新子区 fanout 时补齐。
//
// 旧实现的 bug：enumerate 在 tx 外、auto_follow=1 写入之前；在 enumerate 与
// commit 之间创建的子区会被永久遗漏 —— OnThreadCreated 看不到 auto_follow=1，
// enumerate 的快照也没拿到该子区。
//
// follow_version 在 Phase 1 与 Phase 3 各 +1 —— 总次数随路径而定：未注入
// enumerator / 群下无 active 子区 / Phase 3 re-check 跳过 都让 bump 停在 +1。
// 关键不变量是 bump 次数与子区数量 N 无关（不会随 N 线性增长），保持小常数。
//
// 当未注入 ThreadEnumerator 时（单测 / 迁移期）跳过 Phase 2/3，仅写群行。
func (s *Service) FollowChannel(uid, spaceID, groupNo string) error {
	if err := validateBase(uid, spaceID); err != nil {
		return err
	}
	if groupNo == "" {
		return errors.New("group_no must not be empty")
	}

	// PR #123 round-1 review (Jerry-Xin / yujiawei P1)：FollowChannel 已不再是
	// inert 的"清自己黑名单"，会写 auto_follow_threads=1 + 物化既有子区 +
	// 挂 OnThreadCreated 订阅。必须在任何 DB 写入之前校验 caller 是该 group
	// 的成员、且该群在请求 Space 可见，否则会泄露同 Space 内私有群的子区元数据。
	// nil checker 仅用于单测 / 迁移期。
	if checker := s.getChannelAuthChecker(); checker != nil {
		if err := checker.AuthorizeChannelFollow(uid, spaceID, groupNo); err != nil {
			return err
		}
	}

	zero := int8(0)
	one := int8(1)

	// Phase 1：commit auto_follow=1 + group_unfollowed=0 + bump version。
	// PR #21 review (lml2468 blocker #2)：先锁 follow_version 行再写 ext，与
	// UpdateSort 同序拿锁，避免 (version vs ext) 反向死锁。
	if err := s.withTx("FollowChannel-phase1", func(tx *dbr.Tx) error {
		if _, err := BumpFollowVersionTx(tx, uid, spaceID); err != nil {
			return fmt.Errorf("FollowChannel phase1 bump version: %w", err)
		}
		if err := upsertTx(tx, uid, spaceID, targetTypeGroup, groupNo, ConvExtFields{
			GroupUnfollowed:   &zero,
			AutoFollowThreads: &one,
		}); err != nil {
			return fmt.Errorf("FollowChannel phase1 upsert: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	enum := s.getThreadEnumerator()
	if enum == nil {
		return nil
	}

	// Phase 2：在 Phase 1 commit 之后枚举 —— race-window 已被关闭。
	shortIDs, err := enum.EnumerateActiveShortIDs(groupNo, maxAutoFollowThreadsPerChannel)
	if err != nil {
		// Phase 1 已经 commit；client 仍能观察到 auto_follow=1 + 后续新子区 fanout，
		// 只是初始批的子区未物化。错误向上返回让调用方记录，但不回滚 Phase 1。
		return fmt.Errorf("FollowChannel enumerate threads: %w", err)
	}
	if len(shortIDs) == 0 {
		return nil
	}

	// Phase 3：bulk INSERT IGNORE thread ext + bump version。INSERT IGNORE 让
	// 与并发 OnThreadCreated 的重叠安全（同 (uid, target_type=5, target_id)
	// 由 UK 守护，重复写不会产生第二行）。
	//
	// Bug fix B2 (yujiawei P2 / lml2468 round-2 #2)：在 Phase 1 commit 与 Phase 3
	// 之间用户可能调用 UnfollowChannel，那一刻群行变成 auto_follow=0 + group_unfollowed=1。
	// Phase 3 必须在同一 tx 内 SELECT ... FOR UPDATE 该群行重新确认状态，发现不再
	// 资格则跳过整批写入（含 bump），避免重建已被 UnfollowChannel 清掉的孤立 thread 行。
	return s.withTx("FollowChannel-phase3", func(tx *dbr.Tx) error {
		eligible, err := isChannelStillAutoFollowedTx(tx, uid, spaceID, groupNo)
		if err != nil {
			return fmt.Errorf("FollowChannel phase3 recheck: %w", err)
		}
		if !eligible {
			// 用户已在 Phase 1 与 Phase 3 之间取关；丢弃旧 enumerate 快照，不写任何东西。
			return nil
		}
		if _, err := BumpFollowVersionTx(tx, uid, spaceID); err != nil {
			return fmt.Errorf("FollowChannel phase3 bump version: %w", err)
		}
		if err := bulkInsertIgnoreThreadExtTx(tx, uid, spaceID, groupNo, shortIDs); err != nil {
			return fmt.Errorf("FollowChannel phase3 materialize threads: %w", err)
		}
		return nil
	})
}

// isChannelStillAutoFollowedTx 在 tx 内对 (uid, spaceID, target_type=2, groupNo)
// 取 SELECT ... FOR UPDATE，判断当前是否仍处于"已关注 + 自动跟随子区"状态。
// 行不存在或两标志中任一不满足都返回 false，让 FollowChannel Phase 3 跳过写入。
// 锁顺序：本函数应当在 BumpFollowVersionTx 之后调用 —— 群 ext 行的 SELECT
// FOR UPDATE 不能先于 user_follow_version 的 X 锁，否则与 UpdateSort 反向死锁。
// 但 Phase 3 实际是先 SELECT 再 bump（bump 只在 eligible 时发生），
// 这是个例外：Phase 1 已经先锁过 user_follow_version 并 commit，本 tx 重新进入
// 时 follow_version 行尚未在本 tx 中被锁，并不存在与 UpdateSort 反向交叉的窗口。
func isChannelStillAutoFollowedTx(tx *dbr.Tx, uid, spaceID, groupNo string) (bool, error) {
	var row struct {
		AutoFollow int8 `db:"auto_follow_threads"`
		Unfollowed int8 `db:"group_unfollowed"`
	}
	err := tx.SelectBySql(
		"SELECT auto_follow_threads, group_unfollowed FROM "+table+
			" WHERE uid=? AND space_id=? AND target_type=? AND target_id=? FOR UPDATE",
		uid, spaceID, targetTypeGroup, groupNo,
	).LoadOne(&row)
	if err == dbr.ErrNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return row.AutoFollow == 1 && row.Unfollowed == 0, nil
}

// bulkInsertIgnoreThreadExtTx 给 (uid, spaceID) 在 user_conversation_ext 表中
// 批量插入 target_type=5 子区行。已存在的行（含用户手动调过 follow_sort 的）
// 因 INSERT IGNORE 保持不变 —— 这是 FollowChannel 既不覆盖用户既有排序也能
// 一次性补齐缺失行的关键。
func bulkInsertIgnoreThreadExtTx(tx *dbr.Tx, uid, spaceID, groupNo string, shortIDs []string) error {
	if len(shortIDs) == 0 {
		return nil
	}
	placeholders := make([]string, len(shortIDs))
	args := make([]interface{}, 0, len(shortIDs)*4)
	for i, sid := range shortIDs {
		placeholders[i] = "(?, ?, ?, ?)"
		args = append(args, uid, spaceID, targetTypeThread, groupNo+threadSeparator+sid)
	}
	_, err := tx.InsertBySql(
		"INSERT IGNORE INTO "+table+
			" (uid, space_id, target_type, target_id) VALUES "+
			strings.Join(placeholders, ", "),
		args...,
	).Exec()
	return err
}

// withTx wraps fn in a tx with consistent error handling.
func (s *Service) withTx(op string, fn func(tx *dbr.Tx) error) error {
	tx, err := s.session.Begin()
	if err != nil {
		return fmt.Errorf("%s begin tx: %w", op, err)
	}
	defer tx.RollbackUnlessCommitted()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s commit: %w", op, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// UnfollowChannel — blacklist a group + cascade-delete its thread ext rows
// ---------------------------------------------------------------------------

// UnfollowChannel marks the group as unfollowed (group_unfollowed=1) and, in
// the same transaction, deletes all thread (target_type=5) ext rows whose
// target_id starts with "{groupNo}____" for this user+space, and bumps the
// user_follow_version (PR review Round-3 Blocking #1/#2).
func (s *Service) UnfollowChannel(uid, spaceID, groupNo string) error {
	if err := validateBase(uid, spaceID); err != nil {
		return err
	}
	if groupNo == "" {
		return errors.New("group_no must not be empty")
	}
	one := int8(1)
	zero := int8(0)
	// PR #21 review (lml2468 blocker #2)：bump 必须先于 ext 行操作，保证与
	// UpdateSort 同序拿锁，避免 (version vs ext) 反向死锁。
	return s.withTx("UnfollowChannel", func(tx *dbr.Tx) error {
		if _, err := BumpFollowVersionTx(tx, uid, spaceID); err != nil {
			return fmt.Errorf("UnfollowChannel bump version: %w", err)
		}
		// 同时清零 auto_follow_threads —— 否则 OnThreadCreated 还会把该用户当作
		// fanout 目标，违反"取消关注 = 不再自动跟随新子区"的语义。
		if err := upsertTx(tx, uid, spaceID, targetTypeGroup, groupNo, ConvExtFields{
			GroupUnfollowed:   &one,
			AutoFollowThreads: &zero,
		}); err != nil {
			return fmt.Errorf("UnfollowChannel upsert group: %w", err)
		}
		if _, err := tx.DeleteBySql(
			"DELETE FROM "+table+
				" WHERE uid=? AND space_id=? AND target_type=? AND target_id LIKE ? ESCAPE '|'",
			uid, spaceID, targetTypeThread, threadLikePrefix(groupNo),
		).Exec(); err != nil {
			return fmt.Errorf("UnfollowChannel delete threads: %w", err)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// OnThreadCreated — fanout on new thread to every user with auto_follow_threads
// ---------------------------------------------------------------------------

// OnThreadCreated 在 thread.Service.CreateThread 提交 tx 之后调用，给所有
// 已对 parent channel 开启 auto_follow_threads=1 的用户物化 thread ext 行
// 并 bump 各自的 follow_version，从而实现"关注 channel 后新建子区自动跟随"。
//
// 设计说明（plan Q1 = C / fanout = 同步）：
//   - 同步执行（非异步队列）—— 客户端 sidebar 在 thread 创建消息送达后立刻能拉到新行。
//   - 单独 tx —— thread.Service.CreateThread 自己的 tx 已 commit，与 IM 频道 / 子区
//     创建消息一样采取 best-effort post-commit hook 风格；fanout 失败只记日志不阻断
//     thread 创建本身。
//   - INSERT IGNORE —— 用户既有的 thread 行（含已手动调过 follow_sort 的）保持不变。
//   - follow_version bump —— 只对真正参与 fanout 的用户 +1。无 auto_follow 用户时整体 no-op。
//   - 锁顺序与 FollowChannel 一致：每个用户先 BumpFollowVersionTx（在 version 行加 X 锁）
//     再写 ext 行，避免与 UpdateSort 反向死锁。
//
// 调用方应在 thread.Service.CreateThread 的 commit 之后立即调用，错误以 wrap 形式上传，
// 由调用方决定是否记日志 / 触发告警（thread 创建不应因 fanout 失败回滚）。
func (s *Service) OnThreadCreated(groupNo, shortID string) error {
	if groupNo == "" {
		return errors.New("group_no must not be empty")
	}
	if shortID == "" {
		return errors.New("short_id must not be empty")
	}

	// 1. 在 tx 外把目标 (uid, space_id) 列表读出 —— 跨 N 用户的 SELECT 不参与 tx 锁。
	var targets []onThreadCreatedTarget
	_, err := s.session.SelectBySql(
		"SELECT uid, space_id FROM "+table+
			" WHERE target_type=? AND target_id=? AND auto_follow_threads=1",
		targetTypeGroup, groupNo,
	).Load(&targets)
	if err != nil {
		return fmt.Errorf("OnThreadCreated query auto-follow users: %w", err)
	}
	if len(targets) == 0 {
		return nil
	}

	threadChannelID := groupNo + threadSeparator + shortID

	// 2. 按 batch 切分，每 batch 一个 tx：
	//    - 单 tx 内仍批量化两条 SQL（bulk bump version + bulk INSERT IGNORE），
	//      避免 per-user round-trip 的 O(N) 延迟。
	//    - batch 之间换 tx 防止两件事：
	//      (a) 单 tx 持锁窗口随 N 线性增长拖慢其它 follow 操作；
	//      (b) bug fix #3 —— 单 SQL 占位符数超过 MySQL 65,535 上限。
	//    - INSERT IGNORE + version 单调递增 让"前一 batch 已 commit、后一 batch
	//      失败"的 partial-success 状态可恢复：下次 FollowChannel / 新建子区
	//      fanout 会把缺失的用户补齐。
	//    锁顺序仍是 follow_version 先于 ext —— 与 FollowChannel / UpdateSort 一致。
	batchSize := onThreadCreatedBatchSize
	if batchSize <= 0 {
		batchSize = 1000
	}
	for start := 0; start < len(targets); start += batchSize {
		end := start + batchSize
		if end > len(targets) {
			end = len(targets)
		}
		batch := targets[start:end]
		if err := s.withTx("OnThreadCreated", func(tx *dbr.Tx) error {
			if err := bulkBumpFollowVersionTx(tx, batch, groupNo); err != nil {
				return fmt.Errorf("OnThreadCreated bulk bump version: %w", err)
			}
			if err := bulkInsertIgnoreThreadExtForUsersTx(tx, batch, groupNo, threadChannelID); err != nil {
				return fmt.Errorf("OnThreadCreated bulk insert thread ext: %w", err)
			}
			return nil
		}); err != nil {
			return fmt.Errorf("OnThreadCreated batch [%d,%d): %w", start, end, err)
		}
	}
	return nil
}

// onThreadCreatedTarget 是 OnThreadCreated 初始 SELECT 出来的目标 (uid, space_id)。
type onThreadCreatedTarget = struct {
	UID     string `db:"uid"`
	SpaceID string `db:"space_id"`
}

// bulkBumpFollowVersionTx 批量给当前仍处于"auto_follow=1 + group_unfollowed=0"
// 状态的 (uid, space_id) +1 follow_version；不在该状态的目标跳过。
//
// Bug fix B2：用 INSERT ... SELECT 内嵌 WHERE 让 re-check 与 write 落在同一条
// SQL，关闭"初始 SELECT 后用户取关"的 race。SELECT 源是 user_conversation_ext
// 的群行（target_type=2），这里 ORDER BY (uid, space_id) 让并发 fanout 的行锁
// 顺序一致（W2，lml2468 round-1）—— 避免两条 OnThreadCreated 不同顺序枚举时
// 触发 InnoDB 死锁检测器回滚。
//
// IN tuple 列表把行集限定在本 batch 内，让单条 SQL 的占位符数受 batchSize 约束。
func bulkBumpFollowVersionTx(tx *dbr.Tx, targets []onThreadCreatedTarget, groupNo string) error {
	if len(targets) == 0 {
		return nil
	}
	tupleHolders := make([]string, len(targets))
	args := []interface{}{targetTypeGroup, groupNo}
	for i, t := range targets {
		tupleHolders[i] = "(?, ?)"
		args = append(args, t.UID, t.SpaceID)
	}
	_, err := tx.InsertBySql(
		"INSERT INTO "+followVersionTable+" (uid, space_id, version)"+
			" SELECT uid, space_id, 1 FROM "+table+
			" WHERE target_type=? AND target_id=?"+
			" AND auto_follow_threads=1 AND group_unfollowed=0"+
			" AND (uid, space_id) IN ("+strings.Join(tupleHolders, ", ")+")"+
			" ORDER BY uid, space_id"+
			" ON DUPLICATE KEY UPDATE version = version + 1",
		args...,
	).Exec()
	return err
}

// bulkInsertIgnoreThreadExtForUsersTx 给本 batch 内当前仍 auto_follow=1 +
// group_unfollowed=0 的 (uid, space_id) 写入 target_type=5 的 thread ext 行。
// Re-check 内嵌在 SELECT WHERE 里 —— 同 bulkBumpFollowVersionTx 关闭 B2 race。
func bulkInsertIgnoreThreadExtForUsersTx(tx *dbr.Tx, targets []onThreadCreatedTarget, groupNo, threadChannelID string) error {
	if len(targets) == 0 {
		return nil
	}
	tupleHolders := make([]string, len(targets))
	args := []interface{}{targetTypeThread, threadChannelID, targetTypeGroup, groupNo}
	for i, t := range targets {
		tupleHolders[i] = "(?, ?)"
		args = append(args, t.UID, t.SpaceID)
	}
	_, err := tx.InsertBySql(
		"INSERT IGNORE INTO "+table+
			" (uid, space_id, target_type, target_id)"+
			" SELECT uid, space_id, ?, ? FROM "+table+
			" WHERE target_type=? AND target_id=?"+
			" AND auto_follow_threads=1 AND group_unfollowed=0"+
			" AND (uid, space_id) IN ("+strings.Join(tupleHolders, ", ")+")"+
			" ORDER BY uid, space_id",
		args...,
	).Exec()
	return err
}

// ---------------------------------------------------------------------------
// FollowThread — re-follow parent group (implicit) + upsert thread ext row
// ---------------------------------------------------------------------------

// FollowThread creates (or ensures) an ext row for the given thread channel,
// and simultaneously clears the parent group's unfollowed flag so that
// following a specific thread implicitly re-follows its parent group.
//
// threadChannelID must have the format "{groupNo}____{shortID}".
//
// PR review (Round 3) Blocking #3: prior to any DB write, the registered
// ThreadAuthChecker (if any) MUST authorise (uid, groupNo, shortID). Without
// this check FollowThread accepted any syntactically valid channel ID and wrote
// an ext row referencing a thread the user could not see — surfacing unauthorised
// entries on subsequent sidebar queries.  ErrThreadForbidden bubbles up unchanged
// for the handler to translate to a 403 response.
func (s *Service) FollowThread(uid, spaceID, threadChannelID string) error {
	if err := validateBase(uid, spaceID); err != nil {
		return err
	}
	groupNo, shortID, err := parseThreadChannelID(threadChannelID)
	if err != nil {
		return err
	}

	if checker := s.getThreadAuthChecker(); checker != nil {
		if err := checker.AuthorizeThreadFollow(uid, spaceID, groupNo, shortID); err != nil {
			return err
		}
	}

	return s.withTx("FollowThread", func(tx *dbr.Tx) error {
		// PR #21 review (lml2468 blocker #2)：先 bump 后改 ext，与 UpdateSort 同序拿锁。
		if _, err := BumpFollowVersionTx(tx, uid, spaceID); err != nil {
			return fmt.Errorf("FollowThread bump version: %w", err)
		}
		// 1. Clear parent group's unfollowed flag.
		zero := int8(0)
		if err := upsertTx(tx, uid, spaceID, targetTypeGroup, groupNo, ConvExtFields{
			GroupUnfollowed: &zero,
		}); err != nil {
			return fmt.Errorf("FollowThread clear parent group: %w", err)
		}
		// 2. Upsert thread ext row (no additional fields — default values suffice).
		if err := upsertTx(tx, uid, spaceID, targetTypeThread, threadChannelID, ConvExtFields{}); err != nil {
			return fmt.Errorf("FollowThread upsert thread: %w", err)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// UnfollowThread — delete thread ext row only
// ---------------------------------------------------------------------------

// UnfollowThread removes the ext row for the given thread channel.
// It does NOT touch the parent group's unfollowed flag.
//
// threadChannelID must have the format "{groupNo}____{shortID}".
// PR review (Round 3) Blocking #1/#2 — bumps follow_version in same tx.
func (s *Service) UnfollowThread(uid, spaceID, threadChannelID string) error {
	if err := validateBase(uid, spaceID); err != nil {
		return err
	}
	if _, _, err := parseThreadChannelID(threadChannelID); err != nil {
		return err
	}
	// PR #21 review (lml2468 blocker #2)：先 bump 后改 ext，与 UpdateSort 同序拿锁。
	return s.withTx("UnfollowThread", func(tx *dbr.Tx) error {
		if _, err := BumpFollowVersionTx(tx, uid, spaceID); err != nil {
			return fmt.Errorf("UnfollowThread bump version: %w", err)
		}
		if _, err := tx.DeleteFrom(table).
			Where("uid=? AND space_id=? AND target_type=? AND target_id=?",
				uid, spaceID, targetTypeThread, threadChannelID).Exec(); err != nil {
			return fmt.Errorf("UnfollowThread delete: %w", err)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// FollowDM — upsert ext row with followed_dm=1
// ---------------------------------------------------------------------------

// FollowDM marks the DM conversation with peerUID as followed (followed_dm=1).
// If categoryID is non-nil the DM is placed into that group_category UUID.
//
// PR #21 Round-6 (Jerry-Xin)：categoryID 类型由 *int64 改为 *string，与
// group_category.category_id (VARCHAR(32) UUID) 一致；DM 与群共用同一分类 namespace
// 由原型 image-v1.png 证实。
// 校验顺序：
//   - 入参合法（uid/spaceID/peerUID 非空）
//   - 事务内 authorizeDMCategoryInTx 校验 categoryID 属于 uid 且 status==1
//     （issue #75：原 DMCategoryChecker 在事务外校验，存在 TOCTOU 窗口；
//     现在挪进 withTx 并配 SELECT ... FOR UPDATE）
//
// PR review (Round 3) Blocking #1/#2 — bumps follow_version in same tx.
func (s *Service) FollowDM(uid, spaceID, peerUID string, categoryID *string) error {
	if err := validateBase(uid, spaceID); err != nil {
		return err
	}
	if peerUID == "" {
		return errors.New("peer_uid must not be empty")
	}
	if categoryID != nil && *categoryID == "" {
		return errors.New("category_id must not be empty string")
	}
	one := int8(1)
	fields := ConvExtFields{
		FollowedDM:   &one,
		DMCategoryID: categoryID,
	}
	// PR #21 review (lml2468 blocker #2)：先 bump 后改 ext，与 UpdateSort 同序拿锁。
	return s.withTx("FollowDM", func(tx *dbr.Tx) error {
		if categoryID != nil {
			if err := authorizeDMCategoryInTx(tx, uid, spaceID, *categoryID); err != nil {
				return err
			}
		}
		if _, err := BumpFollowVersionTx(tx, uid, spaceID); err != nil {
			return fmt.Errorf("FollowDM bump version: %w", err)
		}
		if err := upsertTx(tx, uid, spaceID, targetTypeDM, peerUID, fields); err != nil {
			return fmt.Errorf("FollowDM upsert: %w", err)
		}
		return nil
	})
}

// authorizeDMCategoryInTx validates the category for a DM follow operation
// inside the caller's transaction, holding an X lock on the row in
// group_category that matches category_id (via the uk_category_id unique
// index; InnoDB also locks the corresponding clustered-index row, so this
// serialises against the delete path). Replaces the former
// AuthorizeDMCategory checker (modules/message/1module.go) which ran
// outside the tx and let a concurrent delete commit between the SELECT and
// the upsert.
//
// Lock predicate is a `WHERE category_id=?` UNIQUE-index equality — in
// REPEATABLE READ this takes only a record lock on a hit, avoiding the
// next-key (gap) lock that a non-unique predicate (e.g. `WHERE status=1`)
// would acquire. Status / owner / space checks live in Go for the same
// reason.
//
// Returns ErrDMCategoryForbidden for:
//   - category missing (dbr.ErrNotFound)
//   - status != 1 (deleted)
//   - uid mismatch (not the category owner)
//   - space_id mismatch (category from a different space)
//
// DB errors are wrapped with the function name to mirror the
// `"<op>: %w"` pattern used by FollowChannel / FollowDM bump sites in
// this file, so call-site logs attribute infra failures correctly.
func authorizeDMCategoryInTx(tx *dbr.Tx, uid, spaceID, categoryID string) error {
	var row struct {
		UID     string `db:"uid"`
		SpaceID string `db:"space_id"`
		Status  int    `db:"status"`
	}
	err := tx.SelectBySql(
		"SELECT uid, space_id, status FROM group_category WHERE category_id=? FOR UPDATE",
		categoryID,
	).LoadOne(&row)
	if err != nil {
		if errors.Is(err, dbr.ErrNotFound) {
			return ErrDMCategoryForbidden
		}
		return fmt.Errorf("authorizeDMCategoryInTx: %w", err)
	}
	if row.UID != uid || row.SpaceID != spaceID || row.Status != 1 {
		return ErrDMCategoryForbidden
	}
	return nil
}

// ---------------------------------------------------------------------------
// UnfollowDM — delete ext row
// ---------------------------------------------------------------------------

// UnfollowDM removes the ext row for the DM conversation with peerUID.
// Deleting is cleaner than setting followed_dm=0 because it frees the row
// and avoids stale dm_category_id values.
// PR review (Round 3) Blocking #1/#2 — bumps follow_version in same tx.
func (s *Service) UnfollowDM(uid, spaceID, peerUID string) error {
	if err := validateBase(uid, spaceID); err != nil {
		return err
	}
	if peerUID == "" {
		return errors.New("peer_uid must not be empty")
	}
	// PR #21 review (lml2468 blocker #2)：先 bump 后改 ext，与 UpdateSort 同序拿锁。
	return s.withTx("UnfollowDM", func(tx *dbr.Tx) error {
		if _, err := BumpFollowVersionTx(tx, uid, spaceID); err != nil {
			return fmt.Errorf("UnfollowDM bump version: %w", err)
		}
		if _, err := tx.DeleteFrom(table).
			Where("uid=? AND space_id=? AND target_type=? AND target_id=?",
				uid, spaceID, targetTypeDM, peerUID).Exec(); err != nil {
			return fmt.Errorf("UnfollowDM delete: %w", err)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Internal transaction helpers
// ---------------------------------------------------------------------------

// upsertTx is the transaction-scoped counterpart of DB.Upsert.
// It reuses buildUpsertParts so the SQL construction logic stays in one place.
func upsertTx(tx *dbr.Tx, uid, spaceID string, targetType uint8, targetID string, fields ConvExtFields) error {
	extraCols, extraVals, setClauses, setArgs := buildUpsertParts(fields)

	if len(setClauses) == 0 {
		_, err := tx.InsertBySql(
			"INSERT IGNORE INTO "+table+
				" (uid, space_id, target_type, target_id) VALUES (?, ?, ?, ?)",
			uid, spaceID, targetType, targetID,
		).Exec()
		return err
	}

	colsSQL := "uid, space_id, target_type, target_id"
	if len(extraCols) > 0 {
		colsSQL += ", " + strings.Join(extraCols, ", ")
	}
	placeholders := "?, ?, ?, ?"
	if len(extraVals) > 0 {
		placeholders += strings.Repeat(", ?", len(extraVals))
	}
	setSQL := strings.Join(setClauses, ", ")
	query := fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s) ON DUPLICATE KEY UPDATE %s",
		table, colsSQL, placeholders, setSQL,
	)
	insertArgs := append([]interface{}{uid, spaceID, targetType, targetID}, extraVals...)
	insertArgs = append(insertArgs, setArgs...)
	_, err := tx.InsertBySql(query, insertArgs...).Exec()
	return err
}
