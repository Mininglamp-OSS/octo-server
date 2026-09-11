package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// err.server.workspace.* — Workspace metadata and membership business errors.
// DefaultMessage is the en-US source; the zh-CN runtime translation lives in
// pkg/i18n/locales/active.zh-CN.toml. Internal=true codes never expose their
// source message or details on the wire.
var (
	ErrWorkspaceRequestInvalid = register(codes.Code{
		ID:             "err.server.workspace.request_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid Workspace request.",
		SafeDetailKeys: []string{"field"},
	})
	ErrWorkspaceSpaceRequired = register(codes.Code{
		ID:             "err.server.workspace.space_required",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "A valid organization Space is required.",
	})
	ErrWorkspaceForbidden = register(codes.Code{
		ID:             "err.server.workspace.forbidden",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You do not have permission to access this Workspace.",
	})
	ErrWorkspaceNotFound = register(codes.Code{
		ID:             "err.server.workspace.not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Workspace not found.",
	})
	ErrWorkspaceOwnerProtected = register(codes.Code{
		ID:             "err.server.workspace.owner_protected",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "The Workspace owner must transfer ownership before leaving or being removed.",
	})
	ErrWorkspaceWorkspaceRoleInvalid = register(codes.Code{
		ID:             "err.server.workspace.workspace_role_invalid",
		HTTPStatus:     http.StatusUnprocessableEntity,
		DefaultMessage: "The Workspace role is invalid.",
		SafeDetailKeys: []string{"field"},
	})
	ErrWorkspaceCandidateIneligible = register(codes.Code{
		ID:             "err.server.workspace.candidate_ineligible",
		HTTPStatus:     http.StatusUnprocessableEntity,
		DefaultMessage: "The selected member cannot be added to this Workspace.",
		SafeDetailKeys: []string{"field"},
	})
	ErrWorkspaceDependencyUnavailable = register(codes.Code{
		ID:             "err.server.workspace.dependency_unavailable",
		HTTPStatus:     http.StatusServiceUnavailable,
		DefaultMessage: "A required Workspace dependency is unavailable.",
		Internal:       true,
	})
	ErrWorkspaceInternal = register(codes.Code{
		ID:             "err.server.workspace.internal",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Workspace service failed.",
		Internal:       true,
	})
)
