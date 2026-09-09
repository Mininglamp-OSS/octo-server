package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

var (
	ErrAgentMailGatewayUnauthorized = register(codes.Code{
		ID:             "err.server.agent_mail_gateway.unauthorized",
		HTTPStatus:     http.StatusUnauthorized,
		DefaultMessage: "Authentication is required.",
	})
	ErrAgentMailGatewaySpaceRequired = register(codes.Code{
		ID:             "err.server.agent_mail_gateway.space_required",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Select a Space before opening Agent Mail.",
	})
	ErrAgentMailGatewayRequestInvalid = register(codes.Code{
		ID:             "err.server.agent_mail_gateway.request_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid Agent Mail request.",
	})
	ErrAgentMailGatewayPayloadTooLarge = register(codes.Code{
		ID:             "err.server.agent_mail_gateway.payload_too_large",
		HTTPStatus:     http.StatusRequestEntityTooLarge,
		DefaultMessage: "The Agent Mail request is too large.",
	})
	ErrAgentMailGatewayUnavailable = register(codes.Code{
		ID:             "err.server.agent_mail_gateway.unavailable",
		HTTPStatus:     http.StatusServiceUnavailable,
		DefaultMessage: "Agent Mail is temporarily unavailable.",
		Internal:       true,
	})
	ErrAgentMailGatewayBadResponse = register(codes.Code{
		ID:             "err.server.agent_mail_gateway.bad_response",
		HTTPStatus:     http.StatusBadGateway,
		DefaultMessage: "Agent Mail returned an invalid response.",
		Internal:       true,
	})
)
