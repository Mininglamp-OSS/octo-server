package project

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// respond helpers for modules/project. Most sites call
// httperr.ResponseErrorL(c, errcode.ErrProjectXxx, nil, nil) directly; the helpers
// below exist for the shapes that carry a Detail field, so the SafeDetailKeys
// contract stays in one place.
//
// D5: this module uses ResponseErrorL (wire status pinned to 400, the real status
// in error.http_status), not ResponseErrorLWithStatus. The module is new and has no
// client depending on a fixed 400, so WithStatus would be defensible — but it needs
// maintainer sign-off and currently has a handful of users. Switching later is one
// call per handler and the envelope body is identical either way.
//
// Internal=true codes (ErrProjectQueryFailed / ErrProjectStoreFailed) are
// deliberately NOT wrapped: every call site keeps its own p.Error(..., zap.Error(err))
// so ops can debug from logs, while the renderer carries no message on the wire.

// errSharedAuthRequired / errSharedForbidden / errSharedParamInvalid cache the
// shared codes so the middleware guards do not pay a registry lookup per miss.
//
// Resolved at package init, and a missing registration panics loudly. That is the
// intended behaviour and it is also the module's single largest blast radius: this
// package is blank-imported into the running server, so an unregistered shared code
// here means the process does not boot at all — no IM service, not just no Project
// endpoints. The startup smoke test in the external test package exists for exactly
// this.
var (
	errSharedAuthRequired = mustLookupSharedCode("err.shared.auth.required")
	errSharedForbidden    = mustLookupSharedCode("err.shared.auth.forbidden")
	errSharedParamInvalid = mustLookupSharedCode("err.shared.param.invalid")
)

func mustLookupSharedCode(id string) codes.Code {
	c, ok := codes.Lookup(id)
	if !ok {
		panic("modules/project: shared code not registered: " + id)
	}
	return c
}

// respondProjectRequestInvalid covers the bind-failure / empty-required-field shape. An
// empty field name is omitted so the renderer does not surface a noisy empty key.
//
// Handlers in this module bind with ShouldBindJSON rather than the repo-common
// BindJSON. BindJSON calls gin's AbortWithError(400), which writes the 400 header
// before the handler gets to respond; the envelope body then lands underneath a status
// gin chose. Today that is invisible because ResponseErrorL pins the wire status to 400
// anyway (D5) — but it means the transport status is not ours, and it breaks the moment
// this module moves to ResponseErrorLWithStatus.
func respondProjectRequestInvalid(c *wkhttp.Context, field string) {
	details := i18n.Details{}
	if field != "" {
		details["field"] = field
	}
	httperr.ResponseErrorL(c, errcode.ErrProjectRequestInvalid, nil, details)
}

// respondProjectNameInvalid surfaces the name cap so a client can render a
// localized hint without hard-coding the limit.
func respondProjectNameInvalid(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errcode.ErrProjectNameInvalid, nil, i18n.Details{
		"field":     "name",
		"max_chars": maxNameChars,
	})
}

// respondProjectFieldTooLong surfaces the offending field and its cap.
func respondProjectFieldTooLong(c *wkhttp.Context, field string, maxChars int) {
	httperr.ResponseErrorL(c, errcode.ErrProjectFieldTooLong, nil, i18n.Details{
		"field":     field,
		"max_chars": maxChars,
	})
}

// respondProjectBatchTooLarge surfaces the per-request member batch cap.
func respondProjectBatchTooLarge(c *wkhttp.Context, max int) {
	httperr.ResponseErrorL(c, errcode.ErrProjectBatchTooLarge, nil, i18n.Details{
		"max": max,
	})
}

// respondProjectQuota surfaces which quota was hit and its configured value.
// One helper for all four so the SafeDetailKeys contract ("max") lives once.
func respondProjectQuota(c *wkhttp.Context, code codes.Code, max int) {
	httperr.ResponseErrorL(c, code, nil, i18n.Details{"max": max})
}

// respondProjectNotFound is the single anti-enumeration answer for "does not
// exist", "is in a Space you are not a member of" and "is unlisted and you are not
// a member". It deliberately passes no Details, so the three cases are
// byte-identical on the wire; the distinguishing reason belongs in logs only.
func respondProjectNotFound(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errcode.ErrProjectNotFound, nil, nil)
}

// respondNotLoggedIn / respondForbidden / respondParamInvalid render the shared
// envelopes from middleware. Middleware MUST call c.Abort() after these —
// ResponseErrorL writes the response but does not stop the gin chain.
func respondNotLoggedIn(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errSharedAuthRequired, nil, nil)
}

func respondForbidden(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errSharedForbidden, nil, nil)
}

func respondParamInvalid(c *wkhttp.Context, field string) {
	details := i18n.Details{}
	if field != "" {
		details["field"] = field
	}
	httperr.ResponseErrorL(c, errSharedParamInvalid, nil, details)
}

// respondQueryFailed / respondStoreFailed render the Internal=true envelopes. The
// renderer hides the message and details, so the CALLER must have logged the cause
// with zap.Error before calling these — otherwise the failure leaves no trace
// anywhere.
func respondQueryFailed(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errcode.ErrProjectQueryFailed, nil, nil)
}

func respondStoreFailed(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errcode.ErrProjectStoreFailed, nil, nil)
}
