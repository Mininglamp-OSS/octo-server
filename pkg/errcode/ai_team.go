package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

var (
	ErrAITeamRequestInvalid      = register(codes.Code{ID: "err.server.ai_team.request_invalid", HTTPStatus: http.StatusBadRequest, DefaultMessage: "Invalid AI team request.", SafeDetailKeys: []string{"field", "max_chars"}})
	ErrAITeamDisabled            = register(codes.Code{ID: "err.server.ai_team.disabled", HTTPStatus: http.StatusForbidden, DefaultMessage: "The AI team feature has been disabled by the administrator."})
	ErrAITeamForbidden           = register(codes.Code{ID: "err.server.ai_team.forbidden", HTTPStatus: http.StatusForbidden, DefaultMessage: "You do not have permission to use this AI in the current space."})
	ErrAITeamNotFound            = register(codes.Code{ID: "err.server.ai_team.not_found", HTTPStatus: http.StatusNotFound, DefaultMessage: "AI team resource not found."})
	ErrAITeamIdempotencyConflict = register(codes.Code{ID: "err.server.ai_team.idempotency_conflict", HTTPStatus: http.StatusConflict, DefaultMessage: "The idempotency key was already used for a different request."})
	ErrAITeamContainerProtected  = register(codes.Code{ID: "err.server.ai_team.container_protected", HTTPStatus: http.StatusForbidden, DefaultMessage: "This AI session container cannot be changed through group APIs."})
	ErrAITeamQueryFailed         = register(codes.Code{ID: "err.server.ai_team.query_failed", HTTPStatus: http.StatusInternalServerError, DefaultMessage: "Failed to query AI team data.", Internal: true})
	ErrAITeamStoreFailed         = register(codes.Code{ID: "err.server.ai_team.store_failed", HTTPStatus: http.StatusInternalServerError, DefaultMessage: "Failed to save AI team data.", Internal: true})
	ErrAITeamIMUnavailable       = register(codes.Code{ID: "err.server.ai_team.im_unavailable", HTTPStatus: http.StatusServiceUnavailable, DefaultMessage: "The AI session is not ready. Please retry.", Internal: true})
)
