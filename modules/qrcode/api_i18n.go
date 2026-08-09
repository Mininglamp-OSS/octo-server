package qrcode

import (
	"errors"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

var (
	errQRCodeUnsupportedType     = errors.New("qrcode: unsupported type")
	errQRCodeGroupDataInvalid    = errors.New("qrcode: group data invalid")
	errQRCodeGroupNotFound       = errors.New("qrcode: group not found")
	errQRCodeGroupSpaceForbidden = errors.New("qrcode: group space forbidden")
	errQRCodeScanLoginDisabled   = errors.New("qrcode: scan login disabled")
	errQRCodeInternalQueryFailed = errors.New("qrcode: internal query failed")
	errQRCodeInternalStoreFailed = errors.New("qrcode: internal store failed")
)

func respondQRCodeRequestInvalid(c *wkhttp.Context, field string) {
	details := i18n.Details{}
	if field != "" {
		details["field"] = field
	}
	httperr.ResponseErrorL(c, errcode.ErrQRCodeRequestInvalid, nil, details)
}

func respondQRCodeTokenRequired(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errcode.ErrQRCodeTokenRequired, nil, i18n.Details{"field": "token"})
}

func respondQRCodeHandleError(c *wkhttp.Context, err error) {
	switch {
	case errors.Is(err, errQRCodeGroupDataInvalid), errors.Is(err, errQRCodeUnsupportedType):
		respondQRCodeRequestInvalid(c, "code")
	case errors.Is(err, errQRCodeGroupNotFound):
		httperr.ResponseErrorL(c, errcode.ErrQRCodeGroupNotFound, nil, nil)
	case errors.Is(err, errQRCodeGroupSpaceForbidden):
		httperr.ResponseErrorL(c, errcode.ErrQRCodeGroupSpaceForbidden, nil, nil)
	case errors.Is(err, errQRCodeScanLoginDisabled):
		httperr.ResponseErrorL(c, errcode.ErrUserScanLoginDisabled, nil, nil)
	case errors.Is(err, errQRCodeInternalStoreFailed):
		httperr.ResponseErrorL(c, errcode.ErrQRCodeStoreFailed, nil, nil)
	default:
		httperr.ResponseErrorL(c, errcode.ErrQRCodeQueryFailed, nil, nil)
	}
}
