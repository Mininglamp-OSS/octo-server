package card_template_catalog

import (
	"errors"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

func respondCatalogForbidden(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errcode.ErrCardTemplateCatalogForbidden, nil, nil)
}

func respondCatalogRequestInvalid(c *wkhttp.Context, err error) {
	details := i18n.Details{}
	var validationErr *cardtmpl.ArtifactValidationError
	if errors.As(err, &validationErr) {
		if validationErr.Category != "" {
			details["category"] = validationErr.Category
		}
		if validationErr.Document != "" {
			details["document"] = validationErr.Document
		}
	}
	if len(details) == 0 {
		details = nil
	}
	httperr.ResponseErrorL(c, errcode.ErrCardTemplateCatalogRequestInvalid, nil, details)
}

func respondCatalogContentTooLarge(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errcode.ErrCardTemplateCatalogContentTooLarge, nil, nil)
}

func respondCatalogConflict(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errcode.ErrCardTemplateCatalogConflict, nil, nil)
}

func respondCatalogUnavailable(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errcode.ErrCardTemplateCatalogUnavailable, nil, nil)
}

func respondCatalogIntegrityFailure(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errcode.ErrSharedInternal, nil, nil)
}
