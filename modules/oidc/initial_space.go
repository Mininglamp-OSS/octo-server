package oidc

import (
	commonmod "github.com/Mininglamp-OSS/octo-server/modules/common"
	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	"go.uber.org/zap"
)

// autoJoinInitialSpace 把刚由本次请求建出来的 OIDC 账号加进运维配置的初始 Space
// (task oidc-auto-join-initial-space)。
//
// 只允许在**确实建了号**的路径上调用:callback 里 identity 行插入成功的那一支,
// 以及 /bind/create 成功返回之后。老用户重复登录不该走到这里 —— 触发点是"建号",
// 不是"登录"。这条边界同时也是"管理员把人移出初始 Space 之后不会被自动加回去"
// 的实现方式:第二次登录根本进不到这个函数。
//
// **绝不影响登录结果**。调用点都在会话已经签发之后,本函数不返回错误、不写 HTTP
// 响应,任何失败只落到日志和 metricInitialSpaceJoinTotal。理由是这两件事的等级
// 不同:加不进空间是"登录成功但暂时用不了 integration",运维看计数就能修;登录本身
// 失败是 P0。
//
// 下面的 recover 的**准确范围**:它只兜住本 goroutine 同步执行到的部分 ——
// 成员行写入,以及 afterJoinSpace 里同步跑的默认分类初始化与缓存失效。
// afterJoinSpace 另外 `go` 出去两件事(预设群、SpaceMemberJoin 事件),那两条
// goroutine 越过了本 defer,各自在 modules/space 里自带 recover;这里兜不到它们,
// 别把这段注释读成"整条加入链路的 panic 都被接住了"。
//
// 同样地,这个保证是关于**结果**而不是**时延**:本函数同步执行,一次慢查询或锁等待
// 会拖住正在返回的登录响应,只是不会改变它的成败。详见 PR #835 的 P2-2。
//
// o.ctx == nil 的情形只出现在单测构造的 OIDC 里(newTestOIDC 不注入 ctx),等价于
// 功能未配置:直接跳过,让既有 callback 用例的行为与开关关闭时逐字一致。
func (o *OIDC) autoJoinInitialSpace(uid string) {
	defer func() {
		if r := recover(); r != nil {
			metricInitialSpaceJoinTotal.WithLabelValues(string(spacemod.InitialSpaceFailed)).Inc()
			o.Error("自动加入初始 Space panic(已兜住,不影响登录)",
				zap.String("uid", uid), zap.Any("recover", r))
		}
	}()

	if o.ctx == nil || uid == "" {
		return
	}
	// 每次现读快照而不是在 New 里缓存:运维改配置后本实例立即生效,其他实例随
	// SystemSettings 的定时 Reload 在 60s 内收敛。读的是内存快照,没有 DB 开销。
	spaceID := commonmod.EnsureSystemSettings(o.ctx).OIDCInitialSpaceID()
	if spaceID == "" {
		// 功能关闭。刻意不记日志、不计数:关闭态必须与该功能上线前逐字一致。
		return
	}

	outcome, err := spacemod.AutoJoinInitialSpace(o.ctx, uid, spaceID)
	metricInitialSpaceJoinTotal.WithLabelValues(string(outcome)).Inc()

	switch outcome {
	case spacemod.InitialSpaceJoined:
		o.Info("OIDC 新账号已自动加入初始 Space",
			zap.String("uid", uid), zap.String("space_id", spaceID))
	case spacemod.InitialSpaceAlreadyMember:
		// 幂等命中,不是异常,不打日志。
	default:
		// space_full / space_inactive / error 都在这里点名 space_id:运维要能从
		// 一行日志看出是哪个空间配错了或者满了,而不用去翻配置表。
		o.Warn("OIDC 新账号自动加入初始 Space 未完成(登录不受影响)",
			zap.String("uid", uid),
			zap.String("space_id", spaceID),
			zap.String("result", string(outcome)),
			zap.Error(err))
	}
}
