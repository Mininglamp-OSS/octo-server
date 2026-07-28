package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

var (
	ErrCardTemplateCatalogForbidden = register(codes.Code{
		ID:             "err.server.card_template_catalog.forbidden",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "Only a super admin can manage card templates.",
	})
	ErrCardTemplateCatalogRequestInvalid = register(codes.Code{
		ID:             "err.server.card_template_catalog.request_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The card template bundle is invalid.",
		SafeDetailKeys: []string{"category", "document"},
	})
	ErrCardTemplateCatalogContentTooLarge = register(codes.Code{
		ID:             "err.server.card_template_catalog.content_too_large",
		HTTPStatus:     http.StatusRequestEntityTooLarge,
		DefaultMessage: "The card template request is too large.",
	})
	ErrCardTemplateCatalogConflict = register(codes.Code{
		ID:             "err.server.card_template_catalog.conflict",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "The card template version conflicts with an immutable artifact.",
	})
	ErrCardTemplateCatalogUnavailable = register(codes.Code{
		ID:             "err.server.card_template_catalog.unavailable",
		HTTPStatus:     http.StatusServiceUnavailable,
		DefaultMessage: "The card template catalog is temporarily unavailable.",
		Internal:       true,
	})
)
