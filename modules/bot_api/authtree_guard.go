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

// appBotDMSpaceGuard 把 scope=space 的 App Bot 的 DM 读限制在它自己那个 Space 内。
//
// 为什么读侧需要单独一道门：复用的 getPersonMessage 只走 checkPersonDMAccess，那条链对
// 真人对端是「同 Space **或** 好友」二选一，命中好友就放行，从不看 CtxKeyBotKind /
// CtxKeyAppBotScope / CtxKeyAppBotSpaceID。发送侧不是这样——checkSendPermission 的
// BotKindApp 规则 3（见 send.go）在对端已不是 App Bot 所属 Space 的成员时明确 fail-closed。
// 于是同一个 scope=space App Bot：只要还留着一条 friend 行，就不能再给某人发消息，却仍能
// 按 message_id 把跟这个人的历史 DM 正文读出来。读比写宽是授权 bypass，本 guard 把读侧
// 补齐到与写侧同一条规则（PR #713 review round 4，Jerry-Xin 🔴 Critical）。
//
// 挂载位置与生效范围：挂在 bot 树的 authtree 挂载点上（与 botActorUID 同一条 before 链），
// 因此对 bot 树上每条复用路由都会跑一遍，但只在路由带 :peer_uid 时才做判定——群 / 子区读
// 没有 DM 对端可判，直接放行，它们的边界是 requireGroupMember 的群成员资格。**将来任何
// 新增的 DM 形状 bot 路由必须沿用 peer_uid 这个参数名**，否则会静默漏过这道门；这正是本
// 轮 review 里 authtree.Route.Tenant 声明要防的那类漂移，故在此写明。
//
// 拒绝一律折成 not-found（与 getPersonMessage 自己所有不可见分支同码），不回显「这个人存在
// 但不在你的 Space」——那会给 App Bot 一条跨 Space 成员探测信道。
func (ba *BotAPI) appBotDMSpaceGuard() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		peerUID := c.Param(peerUIDField)
		if peerUID == "" {
			c.Next()
			return
		}
		kind, _ := c.Get(CtxKeyBotKind)
		if k, _ := kind.(string); k != BotKindApp {
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
