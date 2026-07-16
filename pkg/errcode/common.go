package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

var (
	// ErrSpaceWelcomeConfigInvalid is returned by the manager system_setting
	// write path when the onboarding space-welcome five-tuple does not form a
	// valid *enabled* combination — a missing/dissolved target Space, an
	// unparseable RFC3339 active_from, or a message body that is empty (after
	// trim) or exceeds the code-point limit. The `field` detail names the first
	// offending key so the admin UI can point at it; the specific reason stays
	// generic on the wire (log carries the rest).
	ErrSpaceWelcomeConfigInvalid = register(codes.Code{
		ID:             "err.server.common.space_welcome_config_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid space welcome configuration.",
		SafeDetailKeys: []string{"field"},
	})
)
