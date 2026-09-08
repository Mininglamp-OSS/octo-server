package space

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

// 「关闭某个 uid 在所有 Space 的席位」的对外入口。
//
// # 为什么需要它
//
// 一个账号可能不是被某个 Space 的管理员踢掉，而是**整体消失**——今天唯一这样的
// 账号是被 BotFather 删除的 Bot。那条路径原先直接写
// `UPDATE space_member SET status=0 WHERE uid=? AND status=1`：一条语句改掉所有
// Space 的席位，不写任何清理工单。
//
// 在 Bot 只能进群、不能进项目的年代，这只是「少跑一遍已经手工跑过的群清理」。
// 但项目层把 space_member 变成了**授权链的根**：
//
//	space_member.status=1  →（P0 级联）octo_project_member.status=1
//	                       →（P1 准入闸门）可以留在项目群里
//
// 于是那条裸 UPDATE 变成一个真实缺陷：Bot 被删除，Space 席位关了，而
// 项目席位没有任何东西去关——它会永远停在 status=1，I1 对账扫描从此每一轮都报
// 一条永远修不好的违规，而这个已经不存在的账号在项目成员名单里还活着。
//
// 修法不是在 BotFather 里再补一段项目清理（那是第二次重新实现级联，而重新实现
// 级联正是本仓库反复付过学费的事），而是让它走**所有其它移除路径都走的那条路**：
// 在关席位的同一个事务里写出清理工单，剩下的交给已有的 worker 和已注册的步骤。
//
// # 与 removeMemberLocked 的关系
//
// 语义上这是它的「跨 Space 批量版」，但刻意不复用：
//
//   - removeMemberLocked 带角色守卫（不能移除 owner、不能移除同级或更高角色）。
//     那是**人对人**的授权规则。一个被删除的账号不是在被谁踢，它是不存在了；
//     让它撞上「不能移除 owner」而失败，会留下一个属于已删除账号的 owner 席位。
//   - 它是单 Space 的，而这里的输入根本没有 Space——要先查出来。
//
// 共用的是真正该共用的那部分：**同一事务内关席位 + 入队**，也就是
// enqueueMemberRemovalCleanupTx 本身。
//
// # 一个 Space 一个事务
//
// 不是一个大事务包住所有 Space。工单是按 (space_id, uid) 键的，每个 Space 的清理
// 互不相干；一个大事务会把所有 Space 的 space_member 行锁在一起，而这条路径的
// 调用方（BotFather 的删除命令）本来就不要求跨 Space 原子性——它今天那条裸 UPDATE
// 也只是单条语句的原子性，并不保证「要么所有 Space 都关、要么都不关」。
// 逐 Space 提交还让部分失败保留部分进展：已提交的那些 Space 的工单已经落库。
func closeSeatsAllSpaces(ctx *config.Context, uid, operatorUID, reason string) ([]string, error) {
	if uid == "" {
		return nil, errors.New("space: close all seats requires uid")
	}
	if !IsMemberRemoveReason(reason) {
		return nil, fmt.Errorf("space: unknown member removal reason %q", reason)
	}

	session := ctx.DB()

	// 先无锁读出候选 Space。
	//
	// 这一步只是拿到「要逐个处理哪些 Space」的清单，真正的判定在下面每个事务内
	// 的 `AND status=1` 谓词里——那条 UPDATE 的 RowsAffected 才是「这次真的关了
	// 一个席位」的权威答案。所以这里读到一个刚刚已经被别人关掉的 Space 不会造成
	// 重复入队：下面 affected==0 就跳过。
	//
	// 反过来，读完之后新增的席位不会被本次处理。对「账号被删除」这个语义来说，
	// 那意味着有人在删除进行中给一个正在消失的账号发了 Space 邀请。
	//
	// 那条席位**没有任何扫描会报它**。上一版这里写着"会被 I1 对账扫描报出来"，
	// 那句是错的，而且方向正好反了：I1 是"项目席位还活着但 Space 席位没了"，
	// 定义域是 octo_project_member；这里剩下的恰恰是一条活着的 Space 席位，
	// 而这个账号可能压根没有任何项目席位。PR #855 第五轮 review 的 Q6。
	//
	// 兜住它需要一个跨 Space 的锁，代价见上面「一个 Space 一个事务」。真正的兜底
	// 在调用方：botfather 删 Bot 那条路径在本函数返回 error 时**中止删除**，让用户
	// 重试，而重试是幂等的（已关的席位 affected=0）。
	var spaceIDs []string
	if _, err := session.SelectBySql(
		"SELECT space_id FROM space_member WHERE uid=? AND status=1", uid,
	).Load(&spaceIDs); err != nil {
		return nil, fmt.Errorf("space: query seats to close: %w", err)
	}
	if len(spaceIDs) == 0 {
		return nil, nil
	}

	closed := make([]string, 0, len(spaceIDs))
	var firstErr error
	for _, spaceID := range spaceIDs {
		if spaceID == "" {
			continue
		}
		ok, err := closeOneSeatAndEnqueueTx(ctx, spaceID, uid, operatorUID, reason)
		if err != nil {
			// 单个 Space 失败不中断其余 Space：已提交的那些工单已经落库，
			// 中断反而会让后面那些 Space 连工单都没有。
			//
			// 失败的这个 Space 的席位仍然是 status=1，而**没有任何扫描会报它**。
			// 上一版这里写着"I1 对账会把它报出来"，那句是错的：I1 的定义域是
			// octo_project_member（"项目席位还活着但 Space 席位没了"），而这里
			// 恰好相反——Space 席位活着，项目席位可能压根不存在。
			// 兜底靠调用方：botfather 删 Bot 那条路径在 closeErr 非空时**中止删除**
			// 并让用户重试（robot 行仍可选中），所以这个残留不会静默留存。
			// PR #855 第五轮 review。
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if ok {
			closed = append(closed, spaceID)
		}
	}
	return closed, firstErr
}

// closeOneSeatAndEnqueueTx 在一个事务内关掉一个 Space 的席位并写出清理工单。
//
// 返回 false 表示这次没有改动成员行（席位本来就不在），此时**不入队**——对着一个
// 不存在的席位入队会产出一条永远无事可做的工单，还会让别人的会话面清理被触发一次。
// 这是 removeMemberLocked 与 forceRemove 都遵守的规矩，见 db_manager.go 的注释。
func closeOneSeatAndEnqueueTx(ctx *config.Context, spaceID, uid, operatorUID, reason string) (bool, error) {
	tx, err := ctx.DB().Begin()
	if err != nil {
		return false, fmt.Errorf("space: begin close seat: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	// FOR UPDATE，而不是先读后写。
	//
	// 并发的另一条移除路径（管理端强制移除、成员自助退出）同样是「读一次角色、
	// 再 UPDATE、再入队」。两条路径若都用快照读，两边的 UPDATE 都会各自影响 1 行
	// 吗？不会——MySQL 的 UPDATE 是当前读，第二条会看到 status 已经是 0 而影响
	// 0 行，于是不入队。所以正确性上快照读也能收敛。
	//
	// 取锁是为了另一件事：让两条路径在**同一行**上排队，从而让「关席位」和
	// 「入队」这一对动作对彼此不可分。没有它，A 的 UPDATE 提交后、入队前，B 读到
	// status=0 直接返回 false，两边都认为对方会入队的窗口虽然极窄但是真的。
	var status []int
	if _, err := tx.SelectBySql(
		"SELECT status FROM space_member WHERE space_id=? AND uid=? FOR UPDATE",
		spaceID, uid,
	).Load(&status); err != nil {
		return false, fmt.Errorf("space: lock seat: %w", err)
	}
	if len(status) == 0 || status[0] != 1 {
		return false, nil
	}

	result, err := tx.Update("space_member").
		Set("status", 0).
		Set("updated_at", time.Now()).
		Where("space_id=? AND uid=? AND status=1", spaceID, uid).Exec()
	if err != nil {
		return false, fmt.Errorf("space: close seat: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("space: read close seat result: %w", err)
	}
	if affected == 0 {
		return false, nil
	}

	if err := enqueueMemberRemovalCleanupTx(tx, spaceID, uid, operatorUID, reason); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("space: commit close seat: %w", err)
	}
	return true, nil
}

// ---------- 对外入口 ----------

// removalWorkerKick 让本包之外的调用方能在入队后立刻推一轮 worker。
//
// worker 本身是 10s 定时的（startMemberRemovalCleanupWorker），所以这个钩子只影响
// **时延**，不影响最终一致性——为空时工单最多等一个 tick。做成包级钩子而不是要求
// 调用方持有 *Space，是因为 CloseAllSpaceSeats 的调用方（modules/botfather）拿到的
// 是 *config.Context，而 afterMembersRemoved 那套收尾是 *Space 的方法。
// 同一个手法见 preset_group_admitter.go 的 RegisterPresetGroupAdmitter。
var (
	removalWorkerKickMu sync.RWMutex
	removalWorkerKickFn func()
)

func setRemovalWorkerKick(fn func()) {
	removalWorkerKickMu.Lock()
	defer removalWorkerKickMu.Unlock()
	removalWorkerKickFn = fn
}

func kickRemovalWorker() {
	removalWorkerKickMu.RLock()
	fn := removalWorkerKickFn
	removalWorkerKickMu.RUnlock()
	if fn != nil {
		fn()
	}
}

// CloseAllSpaceSeats 关闭 uid 在所有 Space 的活跃席位，并为每个真的被关掉的席位
// 写出一条会话面清理工单，返回被关掉席位的 Space 列表。
//
// 这是「一个账号整体消失」的唯一正确入口。调用方**不要**自己写
// `UPDATE space_member SET status=0`：那样写会跳过整条级联链（P0 关项目席位、
// P1 从项目群移除、群侧退群、会话扩展清理），而这条链上的每一步都注册在
// RegisterMemberRemovalCleanupStep 里，从这里看不见。
//
// reason 必须是 MemberRemoveReason* 之一。对被删除的 Bot 用
// MemberRemoveReasonBotDeleted —— 群侧级联会按它抑制「被 X 移出群聊」系统消息，
// 那句话对一个整体消失的账号是错的（没有人把它移出这个群）。
//
// 席位关闭本身是**同步**的：函数返回时，被列出的每个 Space 里这个 uid 已经不是
// 活跃成员，鉴权缓存也已清掉。异步的只有工单驱动的那些清理步骤。调用方若在返回后
// 立刻断言 `space_member.status=0`，断言仍然成立。
//
// 部分失败会返回 error 且 closed 非空：已提交的 Space 是真的已提交，没有什么可
// 回滚的，而重试整个函数是幂等的（已关的席位 affected=0，不会重复入队）。
//
// 调用方**不要只记日志就往下走**。上一版这里写着"应当记日志而不是回滚"，而第四轮
// review 恰好把那条行为从唯一的调用方身上删掉了：botfather 删 Bot 现在在
// closeErr 非空时中止删除并让用户重试，正因为往下走会留下一个"Space 席位还活着、
// robot 行已禁用"的残留，而上面那段说明了没有扫描看得见它。正确的处置是**中止
// 本次操作、让它可重试**；"没有什么可回滚的"说的是不必补偿已提交的部分，不是
// 可以当作成功。PR #855 第五轮 review 的 Q6。
func CloseAllSpaceSeats(ctx *config.Context, uid, operatorUID, reason string) (closed []string, err error) {
	closed, err = closeSeatsAllSpaces(ctx, uid, operatorUID, reason)

	// 鉴权缓存**同步**清掉，即使入队过程中出过错。
	//
	// 这是隔离边界，与 afterMembersRemoved 里那段同一个理由：SpaceMiddleware 读的
	// 是 60s TTL 的正向缓存，不清就等于这个已被删除的账号在缓存有效期内还能过门。
	// 已经成功关掉的席位必须清，哪怕别的 Space 失败了。
	if len(closed) > 0 {
		conn := ctx.GetRedisConn()
		if conn != nil {
			logger := log.NewTLog("Space")
			for _, spaceID := range closed {
				cacheErr := spacepkg.InvalidateMembershipCache(conn, spaceID, uid)
				switch {
				case cacheErr == nil:
				case errors.Is(cacheErr, spacepkg.ErrMembershipCacheNegativeFallback):
					// 正向条目没删掉，但已被否定缓存盖住，中间件当场就拒。
					// 边界守住了，记 Warn 让它可查，不要当越权报。
					logger.Warn("清理成员鉴权缓存：DEL 失败，已写否定缓存兜底，隔离仍然生效",
						zap.String("spaceId", spaceID), zap.String("uid", uid), zap.Error(cacheErr))
				default:
					logger.Error("清理成员鉴权缓存失败：被删除账号可能在缓存 TTL 内仍可访问该 Space",
						zap.String("spaceId", spaceID), zap.String("uid", uid), zap.Error(cacheErr))
				}
			}
		}
		// notify 的进程内成员缓存也要清。逐个 Space 一次，与 afterMembersRemoved
		// 同一个调用；不清的话卡片/通知在本进程的 TTL 内仍然会投给这个已经消失的
		// 账号。这一层不是隔离手段（隔离靠上面那份鉴权缓存），但它是每一条移除路径
		// 都做的收尾，而本函数是"一个账号整体消失"的唯一入口——少做一样，Bot 删除
		// 就成了唯一不做它的那条移除。PR #855 第五轮 review 的 Q7。
		for _, spaceID := range closed {
			invalidateSpaceMemberCacheOf(spaceID)
		}

		// SpaceMemberRemove 观察者事件。今天零监听方（fireSpaceMemberRemoveEventOn
		// 在没有监听方时直接返回，一次 DB 都不写），所以这不是修一个现存的 bug，
		// 而是修一个**将来一定会被踩到**的不对称：等哪天有人 AddEventListener，
		// 别的移除路径都会通知他，只有 Bot 删除不会。
		//
		// 必须用 go 发：listener 分支在调用者 goroutine 上同步跑完所有监听方，
		// 而本函数在 HTTP 请求里被调用。与 afterMembersRemoved 一样，**一个**
		// goroutine 串行发完，不是每个 Space 一个。
		go func(spaceIDs []string) {
			defer func() {
				if r := recover(); r != nil {
					log.NewTLog("Space").Error("账号整体移除收尾 panic",
						zap.Any("recover", r), zap.String("uid", uid))
				}
			}()
			for _, spaceID := range spaceIDs {
				fireSpaceMemberRemoveEventOn(ctx, spaceID, uid, operatorUID, reason)
			}
		}(closed)

		// 推一轮 worker，让级联不必等下一个 10s tick。best-effort。
		kickRemovalWorker()
	}
	return closed, err
}
