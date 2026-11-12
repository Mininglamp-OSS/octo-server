package errcode

import (
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
	"net/http"
)

var (
	ErrPermissionInvalid     = register(codes.Code{ID: "err.server.permission.invalid_bundle", HTTPStatus: http.StatusBadRequest, DefaultMessage: "Invalid permission bundle request."})
	ErrPermissionConflict    = register(codes.Code{ID: "err.server.permission.revision_conflict", HTTPStatus: http.StatusConflict, DefaultMessage: "Permission bundle changed. Read the complete bundle and try again."})
	ErrPermissionForbidden   = register(codes.Code{ID: "err.server.permission.scope_forbidden", HTTPStatus: http.StatusForbidden, DefaultMessage: "Permission scope is unavailable to this identity."})
	ErrPermissionReference   = register(codes.Code{ID: "err.server.permission.reference_unavailable", HTTPStatus: http.StatusBadRequest, DefaultMessage: "Permission references are unavailable."})
	ErrPermissionUnavailable = register(codes.Code{ID: "err.server.permission.bundle_unavailable", HTTPStatus: http.StatusServiceUnavailable, DefaultMessage: "Permission bundle is unavailable.", Internal: true})
	ErrPermissionVersion     = register(codes.Code{ID: "err.server.permission.unsupported_version", HTTPStatus: http.StatusBadRequest, DefaultMessage: "Unsupported permission contract version."})
)
