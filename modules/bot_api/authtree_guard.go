package bot_api

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"go.uber.org/zap"
)

// peerUIDField 是 DM 形状路由里对端 UID 的路径参数名，与 modules/message 贡献的
// /messages/person/:peer_uid/:message_id 保持一致。
const peerUIDField = "peer_uid"

// groupNoField 是群 / 子区形状路由里群号的路径参数名。
const groupNoField = "group_no"

// appBotScopeGuard 给 bot 树上 authtree 贡献的复用路由补齐 App Bot 的授权规则。按路由形状
// 分两条，都是把读侧对齐到本模块其它端点已经在执行的策略：
//
//	带 :peer_uid（DM 单条读）  → scope=space 的 App Bot 必须证明对端仍在它绑定的 Space
//	带 :group_no（群/子区读）  → App Bot 一律拒绝（App Bot is DM-only）
//
// 为什么 DM 那条需要单独一道门：复用的 getPersonMessage 只走 checkPersonDMAccess，那条链
// 对真人对端是「同 Space **或** 好友」二选一，命中好友就放行，从不看 CtxKeyBotKind /
// CtxKeyAppBotScope / CtxKeyAppBotSpaceID。发送侧不是这样——checkSendPermission 的
// BotKindApp 规则 3（见 send.go）在对端已不是 App Bot 所属 Space 的成员时明确 fail-closed。
// 于是同一个 scope=space App Bot：只要还留着一条 friend 行，就不能再给某人发消息，却仍能
// 按 message_id 把跟这个人的历史 DM 正文读出来。读比写宽是授权 bypass（PR #713 review
// round 4，Jerry-Xin 🔴 Critical）。
//
// 为什么群那条也要显式拒：本模块每一个既有 bot 群/子区端点都写明拒 App Bot
// （validateBotGroupAccess 的 "App Bot is DM-only"，以及 groups.go 各处的同款内联判定）。
// 这两条新路由今天恰好也到不了——requireGroupMember 要求活跃 group_member 行，而 App Bot
// 没有那种行（见 resolve_targets.go）——但那是运气替代策略。策略要自己写出来，否则
// 「App Bot 只做 DM」这条约束在这棵树上就只剩一个巧合在守。
//
// 挂载位置与生效范围：挂在 bot 树的 authtree 挂载点上（与 botActorUID 同一条 before 链），
// 对每条复用路由都会跑，按上面两个参数名分派；两个都没有时放行。**将来新增的 DM / 群形状
// bot 路由必须沿用 peer_uid / group_no 这两个参数名**，否则会静默漏过这道门；这正是本轮
// review 里 authtree.Route.Tenant 声明要防的那类漂移，故在此写明。
//
// DM 分支的拒绝一律折成 not-found（与 getPersonMessage 自己所有不可见分支同码），不回显
// 「这个人存在但不在你的 Space」——那会给 App Bot 一条跨 Space 成员探测信道。群分支回既有
// 的 ErrBotAPIAppBotUnsupported，与所有 sibling 端点同码：那里没有可泄露的存在性，调用方
// 需要知道的是「App Bot 不支持群操作」。
func (ba *BotAPI) appBotScopeGuard() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		kind, _ := c.Get(CtxKeyBotKind)
		isAppBot := false
		if k, _ := kind.(string); k == BotKindApp {
			isAppBot = true
		}

		if c.Param(groupNoField) != "" {
			if isAppBot {
				httperr.ResponseErrorL(c, errcode.ErrBotAPIAppBotUnsupported, nil, nil)
				c.Abort()
				return
			}
			c.Next()
			return
		}

		peerUID := c.Param(peerUIDField)
		if peerUID == "" || !isAppBot {
			c.Next()
			return
		}
		scope, _ := c.Get(CtxKeyAppBotScope)
		if s, _ := scope.(string); s != "space" {
			c.Next()
			return
		}

		// scope=space 却没落下 space_id 说明认证链装配有误。发送侧同款情况按
		// errBotSendPermCheckFailed fail-closed，读侧同样不能放行。
		spaceIDVal, _ := c.Get(CtxKeyAppBotSpaceID)
		spaceID, _ := spaceIDVal.(string)
		if spaceID == "" {
			ba.Error("scope=space 的 App Bot 缺少 app_bot_space_id，DM 读 fail-closed")
			httperr.ResponseErrorL(c, errcode.ErrMessageNotFound, nil, nil)
			c.Abort()
			return
		}

		isMember, err := ba.isSpaceMember(peerUID, spaceID)
		if err != nil {
			ba.Error("校验 DM 对端是否仍在 App Bot 的 Space 失败", zap.String("space_id", spaceID), zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
			c.Abort()
			return
		}
		if !isMember {
			httperr.ResponseErrorL(c, errcode.ErrMessageNotFound, nil, nil)
			c.Abort()
			return
		}
		c.Next()
	}
}
