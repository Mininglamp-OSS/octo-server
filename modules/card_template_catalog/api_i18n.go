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

func respondCatalogGrantInvalid(c *wkhttp.Context, err error) {
	// 具体拒绝原因只进日志/错误链，响应保持单一 localized envelope（管理面
	// 也不回显任意错误文本，防 details 泄漏内部结构）。
	_ = err
	httperr.ResponseErrorL(c, errcode.ErrCardTemplateCatalogGrantInvalid, nil, nil)
}
