package messages_search

import (
	"context"
	"net"
	"net/url"
	"os"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/olivere/elastic"
)

// respondValidation emits VALIDATION_ERROR with field/reason details.
func respondValidation(c *wkhttp.Context, field, reason string) {
	d := i18n.Details{}
	if field != "" {
		d["field"] = field
	}
	if reason != "" {
		d["reason"] = reason
	}
	httperr.ResponseErrorL(c, errcode.ErrMessagesSearchValidationFailed, nil, d)
}

// respondValidationDetails is the variant for callers that already populated a
// Details map (e.g. with max_length).
func respondValidationDetails(c *wkhttp.Context, d i18n.Details) {
	httperr.ResponseErrorL(c, errcode.ErrMessagesSearchValidationFailed, nil, d)
}

// respondUpstream emits UPSTREAM_UNAVAILABLE (search backend down).
func respondUpstream(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errcode.ErrMessagesSearchUpstreamUnavailable, nil, nil)
}

// respondInternal emits INTERNAL_ERROR — used when something on our side broke
// (DSL bug, unexpected unmarshal failure). Body message is generic.
func respondInternal(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errcode.ErrMessagesSearchInternal, nil, nil)
}

// respondRateLimited emits RATE_LIMITED with optional retry-after.
func respondRateLimited(c *wkhttp.Context, retryAfterSec int) {
	d := i18n.Details{}
	if retryAfterSec > 0 {
		d["retry_after"] = retryAfterSec
	}
	httperr.ResponseErrorL(c, errcode.ErrMessagesSearchRateLimited, nil, d)
}

// respondNotFound emits NOT_FOUND for invisible channels / spaces. resource
// is surfaced via details.resource per spec v4.2 §1.7 ("channel" | "space" |
// "" when caller has no specific resource label). Empty resource omits the
// detail key.
func respondNotFound(c *wkhttp.Context, resource string) {
	d := i18n.Details{}
	if resource != "" {
		d["resource"] = resource
	}
	httperr.ResponseErrorL(c, errcode.ErrMessagesSearchNotFound, nil, d)
}

// respondSearchDisabled emits SEARCH_DISABLED — the deployment has
// OCTO_SEARCH_BACKEND=disabled so no search surface serves. Used by the
// backendGate middleware (and mirrored by the legacy surfaces) so every
// search entry point refuses uniformly instead of 500/panic/leaking.
func respondSearchDisabled(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errcode.ErrMessagesSearchDisabled, nil, nil)
}

// respondDepthExceeded emits DEPTH_EXCEEDED — the cumulative pagination depth
// cap (max_result_window) was reached. Carries the cap so clients can surface
// a meaningful "narrow your search" hint.
func respondDepthExceeded(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errcode.ErrMessagesSearchDepthExceeded, nil, i18n.Details{
		"max_depth": maxPaginationDepth,
	})
}

// classifyOSError categorises an error returned by the olivere/elastic client
// into one of our R2 buckets. Returns the responder helper to invoke.
//
// UPSTREAM_UNAVAILABLE covers anything that points at OS itself being down or
// unreachable: connection refused (*net.OpError), DNS / TLS / generic transport
// errors (*url.Error), elastic's "no client" / "request timeout" sentinels,
// HTTP 5xx / 429, and context.DeadlineExceeded. Everything else (DSL bug, bad
// _source, panics) routes to INTERNAL_ERROR.
func classifyOSError(err error) func(*wkhttp.Context) {
	if err == nil {
		return nil
	}
	if e, ok := err.(*elastic.Error); ok {
		switch {
		case e.Status >= 500:
			return respondUpstream
		case e.Status == 429:
			return func(c *wkhttp.Context) { respondRateLimited(c, 0) }
		default:
			return respondInternal
		}
	}
	if isContextDeadlineExceeded(err) {
		return respondUpstream
	}
	if elastic.IsConnErr(err) || elastic.IsTimeout(err) {
		return respondUpstream
	}
	if isTransportErr(err) {
		return respondUpstream
	}
	return respondInternal
}

// isTransportErr is true when the underlying cause is a transport-layer
// problem (DNS, TLS, refused connection, syscall errno) rather than an OS
// application-level reply. We intentionally type-switch on the wrapped chain
// instead of using errors.Is/As so this stays cheap on the hot path.
func isTransportErr(err error) bool {
	for cur := err; cur != nil; {
		switch e := cur.(type) {
		case *url.Error:
			cur = e.Err
			continue
		case *net.OpError:
			return true
		case net.Error:
			if e.Timeout() {
				return true
			}
		case *os.SyscallError:
			return true
		}
		type unwrapper interface{ Unwrap() error }
		if u, ok := cur.(unwrapper); ok {
			cur = u.Unwrap()
			continue
		}
		break
	}
	return false
}

// isContextDeadlineExceeded compares against context.DeadlineExceeded without
// pulling errors.Is into every callsite (keeps the import surface minimal in
// the helpers above).
func isContextDeadlineExceeded(err error) bool {
	for cur := err; cur != nil; {
		if cur == context.DeadlineExceeded {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		if u, ok := cur.(unwrapper); ok {
			cur = u.Unwrap()
			continue
		}
		break
	}
	return false
}
