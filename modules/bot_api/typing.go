package bot_api

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	octolibredis "github.com/Mininglamp-OSS/octo-lib/pkg/redis"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"go.uber.org/zap"
)

// BotTypingReq is the request for typing.
//
// OnBehalfOf — YUJ-1465 / Mininglamp-OSS/octo-server#108 (OBO v2). When
// non-empty the bot is signalling "the OBO grantor (this uid) is typing
// in this channel", not "the bot is typing". Server validates an active
// OBO grant (grantor=OnBehalfOf, grantee=robotID) AND a per-channel
// scope row (channel_id, channel_type) — same auth contract as
// /v1/bot/sendMessage — before signing the CMDTyping payload with
// `from_uid=OnBehalfOf` instead of the bot's uid. Empty / absent
// preserves the v0/v1 behaviour where typing is always attributed to
// the bot. See modules/bot_api/send.go / checkOBO for the grant +
// scope auth contract; we reuse it verbatim so a bot cannot signal
// typing as a grantor it cannot legitimately send as.
type BotTypingReq struct {
	ChannelID   string `json:"channel_id"`
	ChannelType uint8  `json:"channel_type"`
	OnBehalfOf  string `json:"on_behalf_of,omitempty"`
}

// typing handles POST /v1/bot/typing.
func (ba *BotAPI) typing(c *wkhttp.Context) {
	var req BotTypingReq
	if err := c.BindJSON(&req); err != nil {
		respondBotAPIRequestInvalid(c, "")
		return
	}
	if strings.TrimSpace(req.ChannelID) == "" {
		respondBotAPIRequestInvalid(c, "channel_id")
		return
	}
	if req.ChannelType == 0 {
		respondBotAPIRequestInvalid(c, "channel_type")
		return
	}

	robotID := getRobotIDFromContext(c)
	channelID := ba.resolveSpaceChannelID(robotID, req.ChannelID, req.ChannelType)

	// YUJ-1465 / Mininglamp-OSS/octo-server#108 — OBO v2 typing
	// identity. When `on_behalf_of` is non-empty the bot is signalling
	// typing AS the grantor; we reuse the /v1/bot/sendMessage auth
	// contract (active grant + scope + grantor channel access)
	// verbatim so a bot cannot signal typing as a grantor it cannot
	// legitimately send as. The friend-gate bypass below is gated on
	// the SAME `hasOBOContext` pledge so plain-bot typing cannot
	// piggy-back on an unrelated grant.
	hasOBOContext := strings.TrimSpace(req.OnBehalfOf) != ""

	// Permission check: bot must have access to this channel.
	// PR#82 R7 — the OBO friend-gate bypass is conditional on a
	// validated OBO context; without on_behalf_of typing dispatches
	// AS the bot (CMDTyping carries from_uid=robotID below) so the
	// bypass MUST NOT apply (hasOBOContext=false). Allowing it would
	// let a bot that holds an unrelated grant signal typing in a DM
	// with a user that has not opted in to that bot.
	botKind := getBotKindFromContext(c)
	if err := ba.checkSendPermission(c, botKind, robotID, req.ChannelID, req.ChannelType, hasOBOContext); err != nil {
		respondSendPermissionError(c, err)
		return
	}

	// YUJ-1465 — resolve the typing fromUID. Default = the bot. If
	// the bot is signalling on behalf of a grantor, validate the
	// OBO grant + scope BEFORE we mint the CMD, so a forged
	// on_behalf_of cannot leak a typing event as another user.
	fromUID := robotID
	if hasOBOContext {
		// Mirror sendMessage's grantor-reply DM bypass: when the
		// grantor DMs the persona-clone bot, the bot's typing back
		// must not require a scope row covering "grantor talks to
		// self" (no such scope exists by design — see send.go
		// YUJ-1418). The bypass yields fromUID=robotID (bot signals
		// typing as itself) — same shape as a legacy non-OBO send.
		grantorReplyBypass := false
		if req.ChannelType == common.ChannelTypePerson.Uint8() && req.OnBehalfOf == req.ChannelID {
			hasGrant, err := ba.botHasActiveGrantFrom(robotID, req.OnBehalfOf)
			if err != nil {
				ba.Error("OBO typing grantor-reply bypass lookup failed",
					zap.String("bot", robotID),
					zap.String("grantor", req.OnBehalfOf),
					zap.Error(err))
				httperr.ResponseErrorL(c, errcode.ErrBotAPIOBOInternal, nil, nil)
				return
			}
			grantorReplyBypass = hasGrant
		}
		if !grantorReplyBypass {
			if err := ba.checkOBO(robotID, req.OnBehalfOf, req.ChannelID, req.ChannelType); err != nil {
				if errors.Is(err, ErrOBONotAuthorized) {
					ba.Warn("OBO typing denied: no active grant or scope",
						zap.String("bot", robotID),
						zap.String("on_behalf_of", req.OnBehalfOf),
						zap.String("channel_id", req.ChannelID),
						zap.Uint8("channel_type", req.ChannelType))
					httperr.ResponseErrorL(c, errcode.ErrBotAPIOBONotAuthorized, nil, nil)
					return
				}
				ba.Error("OBO typing check failed", zap.Error(err))
				httperr.ResponseErrorL(c, errcode.ErrBotAPIOBOInternal, nil, nil)
				return
			}
			// Typing fromUID becomes the grantor — the typing CMD
			// surfaces in the channel as "<grantor> is typing", which
			// is the whole point of OBO v2 typing.
			fromUID = req.OnBehalfOf
		}
	}

	// Typing throttle: allow forwarding within 90s window, then suppress.
	// Design: bots get max 3 consecutive 90s typing windows per 180s TTL cycle.
	// After 180s both keys expire and bot can start fresh windows — this is intentional
	// (prevents rapid bursts, not indefinite blocking). Matches original botfather behavior.
	typingStartKey := fmt.Sprintf("typing_start:%s:%s:%d", robotID, channelID, req.ChannelType)
	typingCountKey := fmt.Sprintf("typing_count:%s:%s:%d", robotID, channelID, req.ChannelType)
	const typingMaxDuration = 90
	const typingMaxWindows = 3
	const typingKeyTTL = 180

	// YUJ-1465 — typing handler is now exercised in unit tests that
	// build a piecemeal BotAPI without a ctx (and therefore without a
	// Redis connection). The throttle is intentionally best-effort
	// (dropping the rate-limit shield in test mode does not affect
	// correctness, and Redis outages in prod already silently degrade
	// to "no throttle" anyway), so we guard the lookup behind a nil
	// check. The branch below only fires when both ctx and the
	// resolved Redis handle are non-nil; otherwise we skip throttle
	// entirely and proceed to the CMD dispatch.
	var redisConn *octolibredis.Conn
	if ba.ctx != nil {
		redisConn = ba.ctx.GetRedisConn()
	}
	if redisConn != nil {
		startStr, _ := redisConn.GetString(typingStartKey)
		now := time.Now().Unix()
		if startStr != "" {
			startTime, _ := strconv.ParseInt(startStr, 10, 64)
			if now-startTime > int64(typingMaxDuration) {
				c.ResponseOK()
				return
			}
		} else {
			countStr, _ := redisConn.GetString(typingCountKey)
			if countStr != "" {
				count, _ := strconv.ParseInt(countStr, 10, 64)
				if count >= int64(typingMaxWindows) {
					c.ResponseOK()
					return
				}
			}
			_ = redisConn.SetAndExpire(typingStartKey, fmt.Sprintf("%d", now), time.Duration(typingKeyTTL)*time.Second)
			_, _ = redisConn.Incr(typingCountKey)
			// Unconditional SetExpire: if a prior crash left typingCountKey without TTL, this fixes it.
			// Worst case: TTL refreshed on new window creation (acceptable for typing throttle).
			_ = redisConn.SetExpire(typingCountKey, time.Duration(typingKeyTTL)*time.Second)
		}
	}

	paramChannelID := channelID
	if req.ChannelType == uint8(common.ChannelTypePerson) {
		paramChannelID = robotID
	}
	err := ba.dispatchTypingCMD(config.MsgCMDReq{
		NoPersist:   true,
		CMD:         common.CMDTyping,
		ChannelID:   channelID,
		ChannelType: req.ChannelType,
		Param: map[string]interface{}{
			// YUJ-1465 — fromUID is the OBO grantor when on_behalf_of
			// was honored above; otherwise the bot. The typing CMD's
			// `from_uid` is what surfaces in the channel as the
			// "<X> is typing" identity, so this is the load-bearing
			// substitution for OBO v2 typing.
			"from_uid":     fromUID,
			"channel_id":   paramChannelID,
			"channel_type": req.ChannelType,
		},
	})
	if err != nil {
		ba.Error("发送typing失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPISendFailed, nil, nil)
		return
	}
	c.ResponseOK()
}

// dispatchTypingCMD sends a typing CMD via ctx.SendCMD or the
// test-injected `typingCMDDispatch` hook. The seam exists so unit
// tests can capture the resolved `from_uid` (load-bearing for the
// YUJ-1465 OBO v2 typing identity substitution) without standing up a
// live WuKongIM connection. Production path (nil override) is a
// straight passthrough to ctx.SendCMD.
func (ba *BotAPI) dispatchTypingCMD(req config.MsgCMDReq) error {
	if ba.typingCMDDispatch != nil {
		return ba.typingCMDDispatch(req)
	}
	if ba.ctx == nil {
		// Defensive: should not happen in prod (Route is wired with a
		// real ctx), but a piecemeal-built BotAPI in tests without a
		// hook installed must not NPE. Treat as a silent no-op so the
		// handler returns 200 and surrounding test paths continue.
		return nil
	}
	return ba.ctx.SendCMD(req)
}

// heartbeat handles POST /v1/bot/heartbeat.
func (ba *BotAPI) heartbeat(c *wkhttp.Context) {
	robotID := getRobotIDFromContext(c)
	key := fmt.Sprintf("%s%s", heartbeatKeyPrefix, robotID)
	err := ba.ctx.GetRedisConn().SetAndExpire(key, "1", time.Second*heartbeatTTL)
	if err != nil {
		ba.Error("设置心跳失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPISendFailed, nil, nil)
		return
	}
	c.ResponseOK()
}
