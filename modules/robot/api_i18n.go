package robot

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

// respond helpers for modules/robot. Most migrated sites call
// httperr.ResponseErrorL(c, errcode.ErrRobotXxx, nil, nil) directly; the helpers
// below exist only for the shapes that carry a Detail field (so the
// SafeDetailKeys contract stays in one place) and for the robot-webhook auth
// middleware, which must preserve its real wire status.
//
// Internal=true codes (ErrRobotQueryFailed / ErrRobotStoreFailed /
// ErrRobotSendFailed / ErrRobotUploadFailed / ErrRobotTokenGenFailed /
// ErrRobotAuthCheckFailed) are intentionally NOT wrapped here: each call site
// keeps its existing rb.Error(..., zap.Error(err)) log so ops can debug from
// logs, and the renderer carries no message on the wire.

// respondRobotRequestInvalid covers the common BindJSON-failure / "X 不能为空"
// shape — one code, one optional field detail. An empty field is omitted so the
// renderer does not surface a noisy empty key to clients.
func respondRobotRequestInvalid(c *wkhttp.Context, field string) {
	details := i18n.Details{}
	if field != "" {
		details["field"] = field
	}
	httperr.ResponseErrorL(c, errcode.ErrRobotRequestInvalid, nil, details)
}

// respondRobotContentInvalid covers an invalid message payload / content_edit.
// The offending field name is surfaced; the raw payload never is.
func respondRobotContentInvalid(c *wkhttp.Context, field string) {
	details := i18n.Details{}
	if field != "" {
		details["field"] = field
	}
	httperr.ResponseErrorL(c, errcode.ErrRobotContentInvalid, nil, details)
}

// respondRobotContentTypeUnsupported surfaces the rejected message content type.
func respondRobotContentTypeUnsupported(c *wkhttp.Context, contentType int) {
	httperr.ResponseErrorL(c, errcode.ErrRobotContentTypeUnsupported, nil, i18n.Details{
		"type": contentType,
	})
}

// respondRobotFileTooLarge surfaces the upload size cap (in MB) so the client can
// render a localized hint without hard-coding the limit.
func respondRobotFileTooLarge(c *wkhttp.Context, maxMB int64) {
	httperr.ResponseErrorL(c, errcode.ErrRobotFileTooLarge, nil, i18n.Details{
		"max_mb": maxMB,
	})
}

// respondRobotAuthFailed renders the single anti-enumeration 401 for the robot
// webhook auth middleware, preserving the real 401 wire status (external bot
// adapters branch on HTTP 401, not the D14 fixed-400), then aborts the gin chain
// so the protected handler never runs.
func respondRobotAuthFailed(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrRobotAuthFailed, nil, nil)
	c.Abort()
}

// respondRobotAuthCheckFailed renders the auth-middleware infrastructure failure
// (our DB lookup errored), preserving the real 500 so adapters retry, then aborts
// the chain. Internal=true → the underlying cause must be logged at the call site.
func respondRobotAuthCheckFailed(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrRobotAuthCheckFailed, nil, nil)
	c.Abort()
}

// respondRobotInlineQueryTimeout renders the inline-query long-poll timeout,
// preserving the real 408 so the front-end's retry logic still sees a timeout.
func respondRobotInlineQueryTimeout(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrRobotInlineQueryTimeout, nil, nil)
}

// respondManagerForbidden maps the CheckLoginRole* role-guard failures in
// api_manager.go to the shared 403 forbidden code (the octo-lib guard returns a
// raw zh-CN error that must not reach the wire).
func respondManagerForbidden(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errcode.ErrSharedForbidden, nil, nil)
}
