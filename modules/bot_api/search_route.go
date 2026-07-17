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
	ba.searchHandler.MountSubtree(r, "/v1/bot/messages",
		ba.authBot(),
		ba.botActorUID(),
		appwkhttp.SharedUIDRateLimiter(r, ba.ctx),
		ba.resolveSearchPrincipal,
	)
}

// resolveSearchPrincipal 是 bot 搜索的 principal 解析中间件（authBot 之后、
// searchRateLimiter 之前运行）。它按 on_behalf_of 落 as-bot 或 as-user(OBO) 主体，
// 供后续限流/审计/handler 统一经 principal 读取。
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

	// as-user(OBO)：on_behalf_of 存在 → 以 grantor 身份搜索。grant + scope +
	// grantorCanReadChannel 的实时权限校验（TOCTOU 与发消息侧一致）由 validateSearchOBO
	// 承载——YUJ-53 / #F 复用 obo_check.go 接线；在其落地前本层 fail-closed，绝不放行
	// 未校验的 as-user 搜索。
	botUID := getRobotIDFromContext(c)
	if err := ba.validateSearchOBO(c, botUID, obo); err != nil {
		if errors.Is(err, ErrOBONotAuthorized) {
			ba.Warn("search OBO denied: no active grant/scope",
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
	// spaceID 目前不从 bot 请求携带（bot 无 space_member）；OBO 场景的 Space 归属由 #F
	// 随 grantor 解析并注入。#B 先以空 spaceID 组装载体（在 validateSearchOBO 放行前不
	// 可达此处），待 #F 接线后补全。
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

// validateSearchOBO 是 as-user(OBO) 搜索的实时权限校验挂载点（YUJ-53 / #F）。
// #F 将在此复用 obo_check.go 的 grant + scope + grantorCanReadChannel（TOCTOU 与发消息
// 侧一致）做实时收敛。#B（本层仅入口接线）fail-closed：on_behalf_of 搜索在 #F 落地前
// 一律按未授权拒绝，杜绝放行任何未经校验的 as-user 搜索。返回 ErrOBONotAuthorized 时
// 调用方隐藏 grant 是否存在（与发消息侧一致的存在性隐藏）。
func (ba *BotAPI) validateSearchOBO(c *wkhttp.Context, botUID, grantorUID string) error {
	_ = c
	_ = botUID
	_ = grantorUID
	return ErrOBONotAuthorized
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
