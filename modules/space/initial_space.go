package space

import (
	"errors"
	"fmt"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/go-sql-driver/mysql"
)

// InitialSpaceJoinOutcome 描述一次"自动加入初始 Space"的结果。
//
// 调用方（modules/oidc）直接把它当 Prometheus 的 result label 用，所以取值是
// 稳定的小写下划线常量，改动等于改 dashboard 契约。除 InitialSpaceJoined /
// InitialSpaceAlreadyMember 外都代表没建立成员关系。
type InitialSpaceJoinOutcome string

const (
	// InitialSpaceJoined 本次调用真的写入了成员行，副作用（预设群、默认分类、
	// SpaceMemberJoin 事件、成员缓存失效）已按普通加入路径执行。
	InitialSpaceJoined InitialSpaceJoinOutcome = "ok"
	// InitialSpaceAlreadyMember 该 uid 在这个 Space 已经有成员行（在籍或已被移除）。
	// 没有任何写入，视为成功。
	InitialSpaceAlreadyMember InitialSpaceJoinOutcome = "already_member"
	// InitialSpaceFull 空间人数已达 max_users，没有加入。
	InitialSpaceFull InitialSpaceJoinOutcome = "space_full"
	// InitialSpaceInactive 目标 Space 不存在或已解散/封禁，没有加入。
	InitialSpaceInactive InitialSpaceJoinOutcome = "space_inactive"
	// InitialSpaceFailed 查询或写入出错，没有（或不确定是否）加入。err 非 nil。
	InitialSpaceFailed InitialSpaceJoinOutcome = "error"
)

// AutoJoinInitialSpace 把 uid 作为普通成员（role=0）加入 spaceID 指定的 Space。
//
// 用于「运维配了一个初始 Space，之后 OIDC 建出来的账号自动进这个空间」
// （task oidc-auto-join-initial-space）。调用方必须把失败当成非致命：本函数
// 不做任何 HTTP 响应，返回值只用于打日志和计数。
//
// 与用户主动加入（executeJoinSpace）的三点差异，都是刻意的：
//
//  1. **先校验 Space 活性**。executeJoinSpace 由 handler 在 checkSpaceActive
//     之后调用，自己不查 status；本函数没有那层 handler，配置写入后 Space 才被
//     解散是真实存在的时序，所以必须自己挡，否则会往已解散的空间插成员行。
//
//  2. **绝不复活已被移除的成员**。executeJoinSpace 命中 status=0 的行会
//     reactivate——那是"用户拿着邀请码重新申请加入"的正确语义。这里不是：管理员
//     把人移出初始 Space 之后，任何自动流程都不该把他加回去。所以只要成员行存在
//     （无论 status），一律原样返回 already_member，不写库。
//     当前触发点只有建号那一刻、uid 全新，这条分支理论上打不到；写成拒绝而不是
//     复活，是为了让日后若有人从登录路径复用本函数时，语义仍然是安全的那个。
//
//  3. **绕过审批模式**。join_mode=1 的 Space 走审批是给邀请码路径用的；运维在
//     后台钦定的初始 Space 等价于管理端强制添加，不产生 space_join_apply 行。
//
// 容量上限(max_users)语义与普通加入一致,但**判定在本路径自己的事务里**
// (atomicJoinInitialSpace),不是 atomicAddMemberIfNotFull —— 后者服务于用户主动
// 加入,本函数不经过它。两者各自用加锁 COUNT 保证并发下不超员;要确认这一点请看
// TestAutoJoinInitialSpace_ConcurrentJoinersRespectCapacity,别从函数名推断。
//
// 每次调用构造一个 *Space：New 只是几个结构体字面量加一个共享的 settings 单例，
// 没有连接池或 goroutine，而本函数只在建号那一刻跑一次，不值得为它引一个全局单例。
func AutoJoinInitialSpace(ctx *config.Context, uid, spaceID string) (InitialSpaceJoinOutcome, error) {
	if ctx == nil {
		return InitialSpaceFailed, errors.New("space: auto join initial space: nil ctx")
	}
	if uid == "" || spaceID == "" {
		// 空参数是调用方的 bug，不是运行时状况：让它带着 error 出现在日志里，
		// 而不是静默当成"功能没开"。功能没开的判断在调用方（配置为空即跳过）。
		return InitialSpaceFailed, fmt.Errorf("space: auto join initial space: empty uid(%q) or spaceID(%q)", uid, spaceID)
	}
	return New(ctx).autoJoinInitialSpace(uid, spaceID)
}

func (s *Space) autoJoinInitialSpace(uid, spaceID string) (InitialSpaceJoinOutcome, error) {
	// 判定与写入全在一个事务里完成,活性复核在锁内 —— 见 atomicJoinInitialSpace
	// 对窗口与锁序的说明。这里只负责把结果翻给调用方,以及在**提交之后**跑副作用。
	sp, outcome, err := s.db.atomicJoinInitialSpace(spaceID, uid)
	if err != nil {
		return outcome, err
	}
	if outcome != InitialSpaceJoined {
		return outcome, nil
	}

	// 与普通加入完全相同的副作用：预设群组、默认分类、SpaceMemberJoin 事件
	// （botfather 欢迎语 / notify 空间欢迎语靠它）、成员缓存失效。
	// 走同一个函数而不是重抄一遍，是为了让下游监听方看到的事件形状不因入口而异。
	//
	// 放在事务之外是硬要求:这些副作用会起 goroutine、调外部服务,持在事务里既拉长
	// 锁的持有时间,又会在回滚后留下已经外发的副作用。
	s.afterJoinSpace(uid, spaceID, sp)
	return InitialSpaceJoined, nil
}

// isDuplicateMemberError 判断 MySQL 唯一键冲突（1062）。与 modules/oidc、
// modules/usersecret 等处对 mysql.MySQLError.Number 的判定一致。
func isDuplicateMemberError(err error) bool {
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number == 1062
	}
	return false
}
