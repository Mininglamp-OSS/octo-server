package cliv2

import (
	"net/http"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

type errorDefinition struct {
	status       int
	registeredID string
}

var errorDefinitions = map[string]errorDefinition{
	"AUTH_REQUIRED":          {status: http.StatusUnauthorized, registeredID: "err.shared.auth.required"},
	"FORBIDDEN":              {status: http.StatusForbidden, registeredID: "err.shared.auth.forbidden"},
	"NOT_FOUND":              {status: http.StatusNotFound, registeredID: "err.shared.not_found"},
	"CONFLICT":               {status: http.StatusConflict, registeredID: "err.shared.param.invalid"},
	"DUPLICATE":              {status: http.StatusConflict, registeredID: "err.shared.param.invalid"},
	"VALIDATION_ERROR":       {status: http.StatusBadRequest, registeredID: "err.shared.param.invalid"},
	"PAYLOAD_TOO_LARGE":      {status: http.StatusRequestEntityTooLarge, registeredID: "err.shared.param.invalid"},
	"UNSUPPORTED_MEDIA_TYPE": {status: http.StatusUnsupportedMediaType, registeredID: "err.shared.param.invalid"},
	"CLIENT_VERSION_TOO_OLD": {status: http.StatusUpgradeRequired, registeredID: "err.shared.param.invalid"},
	"RATE_LIMITED":           {status: http.StatusTooManyRequests, registeredID: "err.shared.rate.limited"},
	"INTERNAL_ERROR":         {status: http.StatusInternalServerError, registeredID: "err.shared.internal"},
	"UPSTREAM_UNAVAILABLE":   {status: http.StatusServiceUnavailable, registeredID: "err.shared.internal"},
}

// Respond writes the common OpenAPI success envelope for a new v2 adapter.
func Respond(c *wkhttp.Context, data any) {
	if c == nil {
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	c.JSON(http.StatusOK, map[string]any{"data": data})
}

// Fail writes the common OpenAPI error envelope. code must be an R2 public enum
// and message must be a safe English client-facing message, never an internal
// error string. A mismatched code/status or unknown code is converted to a
// generic internal error.
func Fail(c *wkhttp.Context, status int, code, message string) {
	if c == nil {
		return
	}
	definition, ok := errorDefinitions[code]
	if !ok || status != definition.status {
		code = "INTERNAL_ERROR"
		definition = errorDefinitions[code]
		status = http.StatusInternalServerError
		message = ""
	}
	registered, ok := codes.Lookup(definition.registeredID)
	if !ok {
		code = "INTERNAL_ERROR"
		definition = errorDefinitions[code]
		registered, _ = codes.Lookup(definition.registeredID)
		status = http.StatusInternalServerError
		message = ""
	}
	if registered.Internal {
		message = ""
	}
	c.RenderError(wkhttp.ErrorSpec{
		Code:            code,
		DefaultMessage:  message,
		TransportStatus: status,
		SemanticStatus:  status,
		Internal:        registered.Internal,
	})
}

type errorRenderer struct{}

func (errorRenderer) Render(c *wkhttp.Context, spec wkhttp.ErrorSpec) {
	if c == nil {
		return
	}

	if definition, ok := errorDefinitions[spec.Code]; ok {
		renderPublicError(c, spec, spec.Code, definition)
		return
	}
	if registered, ok := codes.Lookup(spec.Code); ok {
		renderRegisteredError(c, spec, registered)
		return
	}
	renderInternalError(c)
}

// cliV2PathPrefix is the mount prefix of the independent CLI v2 router. The
// shared router selects the v2 envelope at the mount root and below it.
const cliV2PathPrefix = "/v2/cli/"

// NewPathAwareErrorRenderer builds the single error renderer for the shared
// HTTP router that hosts both the legacy surface and the mounted CLI v2 router.
// Requests at /v2/cli and below use the existing v2 OpenAPI envelope; every
// other request is delegated to fallback unchanged, so the legacy i18n
// compatibility envelope stays byte-for-byte as before.
//
// Envelope selection must happen at render time because middleware on the
// shared router (notably the global per-IP rate limiter) can reject a request
// before it is forwarded to the independent CLI v2 router, where the v2
// renderer would otherwise apply.
func NewPathAwareErrorRenderer(fallback wkhttp.ErrorRenderer) wkhttp.ErrorRenderer {
	return pathAwareErrorRenderer{fallback: fallback}
}

type pathAwareErrorRenderer struct {
	fallback wkhttp.ErrorRenderer
}

func (r pathAwareErrorRenderer) Render(c *wkhttp.Context, spec wkhttp.ErrorSpec) {
	path := c.Request.URL.Path
	if path == "/v2/cli" || strings.HasPrefix(path, cliV2PathPrefix) {
		errorRenderer{}.Render(c, spec)
		return
	}
	r.fallback.Render(c, spec)
}

func renderPublicError(c *wkhttp.Context, spec wkhttp.ErrorSpec, code string, definition errorDefinition) {
	status := spec.SemanticStatus
	if status == 0 {
		status = spec.TransportStatus
	}
	if status == 0 {
		status = definition.status
	}
	if status != definition.status || (spec.TransportStatus != 0 && spec.TransportStatus != definition.status) {
		renderInternalError(c)
		return
	}

	registered, ok := codes.Lookup(definition.registeredID)
	if !ok {
		renderInternalError(c)
		return
	}
	message := spec.DefaultMessage
	if registered.Internal || spec.Internal {
		if code == "UPSTREAM_UNAVAILABLE" {
			message = "Upstream service unavailable."
		} else {
			message = "Internal server error."
		}
	} else if message == "" {
		message = registered.DefaultMessage
	}
	details := i18n.Details(spec.Details).FilterBy(registered)
	if registered.Internal || spec.Internal || details == nil {
		details = i18n.Details{}
	}
	writeError(c, status, code, message, details)
}

func renderRegisteredError(c *wkhttp.Context, spec wkhttp.ErrorSpec, registered codes.Code) {
	status := spec.SemanticStatus
	if status == 0 {
		status = registered.HTTPStatus
	}
	if status != registered.HTTPStatus {
		renderInternalError(c)
		return
	}
	code, definition, ok := publicErrorForStatus(status)
	if !ok {
		renderInternalError(c)
		return
	}

	internal := spec.Internal || registered.Internal || code == "INTERNAL_ERROR"
	message := registered.DefaultMessage
	details := i18n.Details(spec.Details).FilterBy(registered)
	if internal {
		if code == "UPSTREAM_UNAVAILABLE" {
			message = "Upstream service unavailable."
		} else {
			message = "Internal server error."
		}
		details = i18n.Details{}
	} else if rendered, err := i18n.Params(spec.Params).Render(registered.DefaultMessage); err == nil && rendered != "" {
		message = rendered
	}
	if status >= http.StatusInternalServerError {
		details = i18n.Details{}
	}

	if details == nil {
		details = i18n.Details{}
	}
	writeError(c, definition.status, code, message, details)
}

func publicErrorForStatus(status int) (string, errorDefinition, bool) {
	var code string
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		code = "VALIDATION_ERROR"
	case http.StatusUnauthorized:
		code = "AUTH_REQUIRED"
	case http.StatusForbidden:
		code = "FORBIDDEN"
	case http.StatusNotFound, http.StatusGone:
		code = "NOT_FOUND"
	case http.StatusConflict:
		code = "CONFLICT"
	case http.StatusRequestEntityTooLarge:
		code = "PAYLOAD_TOO_LARGE"
	case http.StatusUnsupportedMediaType:
		code = "UNSUPPORTED_MEDIA_TYPE"
	case http.StatusUpgradeRequired:
		code = "CLIENT_VERSION_TOO_OLD"
	case http.StatusTooManyRequests:
		code = "RATE_LIMITED"
	case http.StatusInternalServerError:
		code = "INTERNAL_ERROR"
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		code = "UPSTREAM_UNAVAILABLE"
	default:
		return "", errorDefinition{}, false
	}
	return code, errorDefinitions[code], true
}

func renderInternalError(c *wkhttp.Context) {
	writeError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.", i18n.Details{})
}

func writeError(c *wkhttp.Context, status int, code, message string, details i18n.Details) {
	c.JSON(status, map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": message,
			"details": map[string]any(details),
		},
	})
}
