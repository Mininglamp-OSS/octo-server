package bot_api

import (
	"errors"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

// respond helpers for modules/bot_api. Most migrated sites call
// httperr.ResponseErrorL / ResponseErrorLWithStatus directly; the helpers below
// exist for the shapes that carry a Detail field (so the SafeDetailKeys
// contract stays in one place) and for the bot-auth middleware and
// permission-check classifier.
//
// Audience: every bot_api endpoint serves EXTERNAL bot adapters (bf_/app_
// tokens) or the logged-in user (OBO management). Sites that already returned a
// real HTTP status (401/403/404/409/413/502) keep it via ResponseErrorLWithStatus;
// legacy c.ResponseError sites stay at wire 400 (D14).
//
// Internal=true codes (ErrBotAPIQueryFailed / StoreFailed / SendFailed /
// UploadFailed / IMTokenFailed / AuthCheckFailed / OBOInternal / UpstreamFailed)
// never surface their message on the wire — each call site keeps its existing
// ba.Error(..., zap.Error(err)) log so ops can debug from logs.

// respondBotAPIIdentityMissing renders the defensive guard for a handler that
// found no robot_id in context — authBot is mounted on every bot route and must
// have populated it, so an empty value is an internal assertion failure: it is
// logged and rendered as a generic Internal 500 (never a probe-able 4xx).
func (ba *BotAPI) respondBotAPIIdentityMissing(c *wkhttp.Context) {
	ba.Error("robot_id missing in context after authBot")
	httperr.ResponseErrorL(c, errcode.ErrSharedInternal, nil, nil)
}

// respondBotAPIRequestInvalid covers the common BindJSON-failure / "X 不能为空" /
// invalid-format shape — one code, one optional field detail. An empty field is
// omitted so the renderer does not surface a noisy empty key.
func respondBotAPIRequestInvalid(c *wkhttp.Context, field string) {
	details := i18n.Details{}
	if field != "" {
		details["field"] = field
	}
	httperr.ResponseErrorL(c, errcode.ErrBotAPIRequestInvalid, nil, details)
}

// respondBotAPILimitExceeded surfaces the offending list field and its cap so
// the client can render a localized hint without hard-coding the limit.
func respondBotAPILimitExceeded(c *wkhttp.Context, field string, max int) {
	details := i18n.Details{"max": max}
	if field != "" {
		details["field"] = field
	}
	httperr.ResponseErrorL(c, errcode.ErrBotAPILimitExceeded, nil, details)
}

// respondBotAPIContentTooLarge surfaces the content field and its byte cap.
func respondBotAPIContentTooLarge(c *wkhttp.Context, field string, maxSize int) {
	details := i18n.Details{"max_size": maxSize}
	if field != "" {
		details["field"] = field
	}
	httperr.ResponseErrorL(c, errcode.ErrBotAPIContentTooLarge, nil, details)
}

// respondBotAPIFileTooLarge surfaces the upload size cap (in MB) for the
// legacy (wire-400) upload path.
func respondBotAPIFileTooLarge(c *wkhttp.Context, maxMB int64) {
	httperr.ResponseErrorL(c, errcode.ErrBotAPIFileTooLarge, nil, i18n.Details{"max_mb": maxMB})
}

// respondBotAPIPayloadTooLarge surfaces the upload byte cap on the voice proxy,
// preserving the real 413 the external client branches on.
func respondBotAPIPayloadTooLarge(c *wkhttp.Context, maxBytes int64) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIPayloadTooLarge, nil, i18n.Details{"max_bytes": maxBytes})
}

// ---- bot-auth middleware (status-preserving, anti-enumeration) --------------

// respondBotAPIAuthFailed renders the single anti-enumeration 401 for the bot
// auth middleware, preserving the real 401 wire status (external adapters branch
// on HTTP 401, not the D14 fixed-400), then aborts the gin chain so the
// protected handler never runs. The specific reason is logged at the call site,
// never returned.
func respondBotAPIAuthFailed(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIAuthFailed, nil, nil)
	c.Abort()
}

// respondBotAPIAuthCheckFailed renders the auth-middleware infrastructure
// failure (our DB lookup errored), preserving the real 500 so adapters retry
// instead of treating it as a permanent 401, then aborts the chain.
// Internal=true → the underlying cause must be logged at the call site.
func respondBotAPIAuthCheckFailed(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIAuthCheckFailed, nil, nil)
	c.Abort()
}

// respondBotAPIBotUnavailable renders the not-published App Bot guard,
// preserving the real 403, then aborts the chain.
func respondBotAPIBotUnavailable(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIBotUnavailable, nil, nil)
	c.Abort()
}

// ---- checkSendPermission classifier -----------------------------------------

// Sentinel errors returned by checkSendPermission so the call sites
// (sendMessage / readReceipt) can map a permission failure to the right code
// via errors.Is — instead of forwarding a raw errors.New string to the wire.
// Infrastructure failures (DB query / missing space_id / unknown bot kind) are
// logged inside checkSendPermission and collapsed to errBotSendPermCheckFailed.
var (
	errBotSendPermAppBotDMOnly   = errors.New("app bot only supports direct messages")
	errBotSendPermNotFriend      = errors.New("bot is not a friend of this user")
	errBotSendPermConvNotStarted = errors.New("user has not started conversation with this bot")
	errBotSendPermNotGroupMember = errors.New("bot is not a member of this group")
	errBotSendPermNotSpaceMember = errors.New("user is no longer a member of bot's space")
	errBotSendPermBadThreadChan  = errors.New("invalid thread channel_id format")
	errBotSendPermCheckFailed    = errors.New("permission check failed")
)

// respondSendPermissionError maps a checkSendPermission sentinel to the i18n
// envelope. Business denials keep wire 400 (legacy ResponseError parity, D14);
// the infrastructure-failure fallback is an Internal 500 (already logged).
func respondSendPermissionError(c *wkhttp.Context, err error) {
	switch {
	case errors.Is(err, errBotSendPermAppBotDMOnly):
		httperr.ResponseErrorL(c, errcode.ErrBotAPIAppBotDMOnly, nil, nil)
	case errors.Is(err, errBotSendPermNotFriend):
		httperr.ResponseErrorL(c, errcode.ErrBotAPINotFriend, nil, nil)
	case errors.Is(err, errBotSendPermConvNotStarted):
		httperr.ResponseErrorL(c, errcode.ErrBotAPIConversationNotStarted, nil, nil)
	case errors.Is(err, errBotSendPermNotGroupMember):
		httperr.ResponseErrorL(c, errcode.ErrBotAPINotGroupMember, nil, nil)
	case errors.Is(err, errBotSendPermNotSpaceMember):
		httperr.ResponseErrorL(c, errcode.ErrBotAPINotSpaceMember, nil, nil)
	case errors.Is(err, errBotSendPermBadThreadChan):
		respondBotAPIRequestInvalid(c, "channel_id")
	default:
		httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
	}
}
