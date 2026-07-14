package resource_share

import (
	"errors"
	"net/http"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/internal/resourceshare"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

func respondDecodeError(c *wkhttp.Context, err error) {
	var maxBytesError *http.MaxBytesError
	if errors.As(err, &maxBytesError) {
		respondPayloadTooLarge(c)
		return
	}
	respondRequestInvalid(c, "body")
}

func respondRequestInvalid(c *wkhttp.Context, reason string) {
	details := i18n.Details{}
	if reason != "" {
		details["reason"] = reason
	}
	httperr.ResponseErrorL(c, errcode.ErrResourceShareRequestInvalid, nil, details)
}

func respondPayloadTooLarge(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errcode.ErrResourceSharePayloadTooLarge, nil, nil)
}

func respondShareError(c *wkhttp.Context, err error) {
	switch {
	case errors.Is(err, resourceshare.ErrShareDisabled):
		httperr.ResponseErrorL(c, errcode.ErrResourceShareDisabled, nil, nil)
	case errors.Is(err, resourceshare.ErrShareForbidden):
		httperr.ResponseErrorL(c, errcode.ErrResourceShareForbidden, nil, nil)
	case errors.Is(err, resourceshare.ErrIntentReplay):
		httperr.ResponseErrorL(c, errcode.ErrResourceShareReplayConflict, nil, nil)
	case errors.Is(err, resourceshare.ErrProviderRevalidation):
		httperr.ResponseErrorL(c, errcode.ErrResourceShareResourceUnavailable, nil, nil)
	case isInvalidIntentError(err):
		httperr.ResponseErrorL(c, errcode.ErrResourceShareIntentInvalid, nil, nil)
	default:
		respondUnavailable(c)
	}
}

func respondUnavailable(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errcode.ErrResourceShareUnavailable, nil, nil)
}

func isInvalidIntentError(err error) bool {
	return errors.Is(err, resourceshare.ErrIntentInvalid) ||
		errors.Is(err, resourceshare.ErrIntentSignature) ||
		errors.Is(err, resourceshare.ErrProviderNotFound) ||
		errors.Is(err, resourceshare.ErrProviderDisabled) ||
		errors.Is(err, resourceshare.ErrProviderConfig)
}

func isExpectedShareRejection(err error) bool {
	return errors.Is(err, resourceshare.ErrShareDisabled) ||
		errors.Is(err, resourceshare.ErrShareForbidden) ||
		errors.Is(err, resourceshare.ErrIntentReplay) ||
		errors.Is(err, resourceshare.ErrProviderRevalidation) ||
		isInvalidIntentError(err)
}

func shareErrorClass(err error) string {
	switch {
	case errors.Is(err, resourceshare.ErrShareDisabled):
		return "disabled"
	case errors.Is(err, resourceshare.ErrShareForbidden):
		return "forbidden"
	case errors.Is(err, resourceshare.ErrIntentReplay):
		return "replay"
	case errors.Is(err, resourceshare.ErrProviderRevalidation):
		return "resource_unavailable"
	case isInvalidIntentError(err):
		return "intent_invalid"
	default:
		return "internal"
	}
}
