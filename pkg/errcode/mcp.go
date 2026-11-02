package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

var (
	ErrMCPPolicyInvalid = register(codes.Code{
		ID: "err.server.mcp.policy_invalid", HTTPStatus: http.StatusBadRequest,
		DefaultMessage: "Invalid MCP policy request.",
	})
	ErrMCPPolicyConflict = register(codes.Code{
		ID: "err.server.mcp.policy_conflict", HTTPStatus: http.StatusConflict,
		DefaultMessage: "MCP policy changed. Read the current generation and try again.",
	})
	ErrMCPPolicyUnavailable = register(codes.Code{
		ID: "err.server.mcp.policy_unavailable", HTTPStatus: http.StatusServiceUnavailable,
		DefaultMessage: "MCP policy is unavailable.", Internal: true,
	})
)
