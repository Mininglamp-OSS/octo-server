package bot_api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/messages_search"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
	"go.uber.org/zap"
)

// 消息搜索的 bot 入口（YUJ-49 / #B，决策六/七/十）。
//
// 把 messages_search 的全部 _search* 端点挂到 /v1/bot/messages，以 authBot() 鉴权，
// principal 由请求 body 的 on_behalf_of 字段区分：
//   - 无 on_behalf_of → as-bot（principal=user_bot，subjectUID=botUID）。
//   - 有 on_behalf_of → as-user(OBO)（principal=obo，subjectUID=grantorUID）。
//
// 中间件链对齐 messages_search/api.go（SpaceMiddleware 除外——/v1/bot 不挂 Space 门，
// bot 无 space_member、spaceID 走 principal 参数）：
//
//	authBot → botActorUID → SharedUIDRateLimiter → resolveSearchPrincipal → searchRateLimiter → audit → backendGate
//
// botActorUID 把 robot_id 落到 "uid"（与 incoming_webhook 一致），使随后的
// SharedUIDRateLimiter 按 botUID 而非 IP 计量——与「限流按 botUID，防单 bot 打爆」的
// 决策一致；细粒度 searchRateLimiter 同样经 principal.RateLimitKey() 归到 botUID。
// 审计 login_uid 记搜索主体、并在 as-user 时同记 bot_uid + grantor_uid。

// searchOBOFieldProbe 只解析 on_behalf_of，用于在 handler BindJSON 之前区分主体。
// 其余搜索参数留给各 _search* handler 自行绑定。
type searchOBOFieldProbe struct {
	OnBehalfOf string `json:"on_behalf_of"`
}

// mountSearchRoutes 挂载 bot 搜索子树。由 Route() 调用。
func (ba *BotAPI) mountSearchRoutes(r *wkhttp.WKHttp) {
	// YUJ-53 / #F：把 as-user(OBO) 的 scope 门注入共享搜索 Handler。messages_search 的
	// obo gate（oboCanReadChannel / oboEnumerateReadableChannels）逐频道经此 checker 复用
	// 发消息侧的 grant + scope + grantorCanReadChannel 实时权限（TOCTOU 一致）。缺此注入则
	// obo gate 走 errOBONoChecker fail-closed，OBO 搜索会被整体拒绝。
	ba.searchHandler.SetOBOChecker(ba)
	ba.searchHandler.MountSubtree(r, "/v1/bot/messages",
		ba.authBot(),
		ba.botActorUID(),
		appwkhttp.SharedUIDRateLimiter(r, ba.ctx),
		ba.resolveSearchPrincipal,
	)
}

// resolveSearchPrincipal 是 bot 搜索的 principal 解析中间件（authBot 之后、
// searchRateLimiter 之前运行）。它按 on_behalf_of 落 as-bot 或 as-user(OBO) 主体，
// 供后续限流/审计/handler 统一经 principal 读取。App Bot 在两条分支都在主体构造阶段
// fail-closed 拒绝（一期不做）。as-user(OBO) 分支在入口做 channel 无关的 grant 存在性
// fail-fast（validateSearchOBO），逐频道的 scope + TOCTOU 收敛下沉到 messages_search 的
// obo gate（复用注入的 ba.SearchOBOAllowed，见 mountSearchRoutes）。
func (ba *BotAPI) resolveSearchPrincipal(c *wkhttp.Context) {
	obo := parseSearchOnBehalfOf(c)
	if obo == "" {
		// as-bot：以 User Bot 自身身份搜索。App Bot 一期显式拒绝（决策五）。
		p, err := messages_search.AuthenticateUserBot(c)
		if err != nil {
			if errors.Is(err, messages_search.ErrAppBotSearchDenied) {
				ba.Warn("search denied: app bot is not supported (YUJ-49)",
					zap.String("bot", getRobotIDFromContext(c)))
				httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIBotUnavailable, nil, nil)
				c.Abort()
				return
			}
			// robot_id 缺失——authBot 之后不应发生，fail-closed。
			respondBotAPIAuthFailed(c)
			return
		}
		messages_search.SetPrincipal(c, p)
		c.Next()
		return
	}

	// as-user(OBO)：on_behalf_of 存在 → 以 grantor 身份搜索。
	botUID := getRobotIDFromContext(c)

	// App Bot 一期整体不做，且拿不到 OBO grant（grant 只查 robot 表，App Bot 在 app_bot
	// 表）。在主体构造阶段显式 fail-closed 拒绝——与 as-bot 分支一致，而非放行后靠 obo gate
	// 静默返回空，避免 App Bot 搜索被误当作「无结果」。
	if getBotKindFromContext(c) == BotKindApp {
		ba.Warn("search OBO denied: app bot is not supported (YUJ-49)",
			zap.String("bot", botUID))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIBotUnavailable, nil, nil)
		c.Abort()
		return
	}

	// 入口 grant 存在性 fail-fast（channel 无关）：bot 当前是否被 grantor 授权（active=1 且
	// global_enabled=1，与发消息侧 checkOBO 的 grant 门同一谓词）。无 grant → 整体拒绝，
	// 避免把完全未授权的 OBO 搜索降级为「逐频道过滤后 200 空结果」。逐频道的 scope +
	// grantorCanReadChannel 实时权限（TOCTOU 与发消息侧一致）由 messages_search 的 obo gate
	// 经注入的 ba.SearchOBOAllowed 收敛（见 mountSearchRoutes / obo.go）。
	if err := ba.validateSearchOBO(c, botUID, obo); err != nil {
		if errors.Is(err, ErrOBONotAuthorized) {
			ba.Warn("search OBO denied: no active grant",
				zap.String("bot", botUID), zap.String("on_behalf_of", obo))
			httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIOBONotAuthorized, nil, nil)
			c.Abort()
			return
		}
		ba.Error("search OBO check failed", zap.Error(err),
			zap.String("bot", botUID), zap.String("on_behalf_of", obo))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIOBOInternal, nil, nil)
		c.Abort()
		return
	}
	// spaceID 不从 bot 请求携带（bot 无 space_member）。P2P 分支回退到 grantor 好友判定
	// （安全，grantor=创建者）；群/子区经 obo gate 逐频道 ∩ scope 收敛，不依赖 spaceID。
	p, err := messages_search.NewOBOPrincipal(botUID, obo, "")
	if err != nil {
		ba.Warn("search OBO principal build failed",
			zap.String("bot", botUID), zap.String("on_behalf_of", obo), zap.Error(err))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIOBONotAuthorized, nil, nil)
		c.Abort()
		return
	}
	messages_search.SetPrincipal(c, p)
	c.Next()
}

// validateSearchOBO 是 as-user(OBO) 搜索的入口 fail-fast：channel 无关地校验 bot 当前是否
// 被 grantor 授权（active=1 且 global_enabled=1 的 grant 行，与发消息侧 checkOBO 的 grant
// 门同一谓词）。逐频道的 scope + grantorCanReadChannel 实时权限（TOCTOU）不在此层——它需要
// channel 维度，由 messages_search 的 obo gate 经注入的 ba.SearchOBOAllowed 承载（决策九：
// 单频道门与 global allowlist 共用同一谓词）。
//
// 返回约定：
//   - nil                    → grant 存在，principal 可组装；越权收敛交给逐频道 gate。
//   - ErrOBONotAuthorized     → 无 active+enabled grant / 主体缺失 / 自授 → 整体拒绝
//     （存在性隐藏，与发消息侧一致，不泄露 grant 是否存在）。
//   - 其他 err                → 基础设施错误 → 调用方 fail-closed（INTERNAL）。
func (ba *BotAPI) validateSearchOBO(c *wkhttp.Context, botUID, grantorUID string) error {
	_ = c
	if botUID == "" || grantorUID == "" || botUID == grantorUID {
		// 与 checkOBO 一致的 fail-closed：主体缺失 / 自授一律未授权。
		return ErrOBONotAuthorized
	}
	grant, err := ba.oboStoreOrDefault().findActiveGrantByGrantorBot(grantorUID, botUID)
	if err != nil {
		return err
	}
	if grant == nil {
		return ErrOBONotAuthorized
	}
	return nil
}

// parseSearchOnBehalfOf 读取并还原请求 body，解析出 on_behalf_of（缺失 / body 为空 /
// 非法 JSON → 返回 ""，交由后续 handler 的 BindJSON 统一报错）。还原 body 使各 _search*
// handler 能再次 BindJSON 读取完整请求体。
func parseSearchOnBehalfOf(c *wkhttp.Context) string {
	body, err := c.GetRawData()
	if err != nil || len(body) == 0 {
		return ""
	}
	// 还原 body 供 handler 复读（GetRawData 会消费 Request.Body）。
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	var probe searchOBOFieldProbe
	if json.Unmarshal(body, &probe) != nil {
		return ""
	}
	return strings.TrimSpace(probe.OnBehalfOf)
}
