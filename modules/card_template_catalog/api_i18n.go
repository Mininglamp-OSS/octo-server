package card_template_catalog

import (
	"errors"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"go.uber.org/zap"
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

func respondCatalogControlDisabled(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errcode.ErrCardTemplateCatalogControlDisabled, nil, nil)
}

func respondCatalogStateConflict(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errcode.ErrCardTemplateCatalogStateConflict, nil, nil)
}

func respondCatalogBlocked(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errcode.ErrCardTemplateCatalogBlocked, nil, nil)
}

func respondCatalogIntegrityFailure(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errcode.ErrSharedInternal, nil, nil)
}

// respondCatalogNotFound is the single answer B2 gives for "no such template",
// "you may not see it" and "it is blocked". Three distinguishable answers would
// let any authenticated caller enumerate the private catalog by probing IDs
// (D5 anti-enumeration), so the reason stays in the logs and never on the wire.
func respondCatalogNotFound(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errcode.ErrSharedNotFound, nil, nil)
}

// respondCatalogGrantInvalid keeps the wire answer to a single localized
// envelope — the manager plane does not echo arbitrary error text either, so a
// details map cannot leak internal structure — while the specific reason goes
// to the log. Dropping the reason entirely would leave an operator with a
// generic 400 and nothing anywhere to explain it.
func (a *API) respondCatalogGrantInvalid(c *wkhttp.Context, err error) {
	if a != nil && a.logger != nil && err != nil {
		// Warn, not Error: a malformed principal type or an over-long id is an
		// expected 4xx, and logging it at Error would put routine caller
		// mistakes into the same bucket as integrity and availability failures.
		a.logger.Warn("card template grant request rejected",
			zap.String("operation", "grant"), zap.Error(err))
	}
	httperr.ResponseErrorL(c, errcode.ErrCardTemplateCatalogGrantInvalid, nil, nil)
}
