package permissionbundle

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
)

// ExplicitSpace enforces the URL's one explicit selector and any header
// agreement. It does not infer a first membership, trim IDs or trust Host.
func ExplicitSpace(r *http.Request) (string, error) {
	q, e := url.ParseQuery(r.URL.RawQuery)
	if e != nil || len(q) != 1 {
		return "", ErrForbidden
	}
	values := q["space_id"]
	if len(values) != 1 || !ValidNativeID(values[0]) {
		return "", ErrForbidden
	}
	h := r.Header.Values("X-Space-ID")
	if len(h) > 1 || len(h) == 1 && h[0] != values[0] {
		return "", ErrForbidden
	}
	return values[0], nil
}
func RespondError(c *wkhttp.Context, e error) {
	code := errcode.ErrPermissionUnavailable
	switch {
	case errors.Is(e, ErrVersion):
		code = errcode.ErrPermissionVersion
	case errors.Is(e, ErrInvalid):
		code = errcode.ErrPermissionInvalid
	case errors.Is(e, ErrConflict):
		code = errcode.ErrPermissionConflict
	case errors.Is(e, ErrForbidden):
		code = errcode.ErrPermissionForbidden
	case errors.Is(e, ErrReference):
		code = errcode.ErrPermissionReference
	}
	httperr.ResponseErrorL(c, code, nil, nil)
}
func private(c *wkhttp.Context) {
	c.Header("Cache-Control", "private, no-store")
	c.Header("Vary", "Authorization, token")
	c.Header("Pragma", "no-cache")
}
func writeJSON(c *wkhttp.Context, v any, limit int) {
	b, e := json.Marshal(v)
	if e != nil {
		RespondError(c, ErrUnavailable)
		return
	}
	x, e := decodeStrict(bytes.NewReader(b), limit)
	if e != nil {
		RespondError(c, ErrUnavailable)
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", canonical(x))
}

// Serve is called only after the module's existing authentication, identity,
// limiter and preliminary Space/owner middleware. The Store rechecks current
// native authority in its transaction. A nil integration remains unavailable.
func (s *Store) Serve(c *wkhttp.Context, botUID, actor, operation string) {
	private(c)
	if s == nil {
		RespondError(c, ErrUnavailable)
		return
	}
	space, e := ExplicitSpace(c.Request)
	if e != nil {
		RespondError(c, e)
		return
	}
	key := Key{s.Origin, botUID, space}
	if (c.Request.Method == http.MethodPut || c.Request.Method == http.MethodPost) && c.GetHeader("Content-Encoding") != "" {
		RespondError(c, ErrInvalid)
		return
	}
	switch operation {
	case "owner_get":
		p, e := s.Get(c.Request.Context(), key, actor)
		if e != nil {
			RespondError(c, e)
			return
		}
		writeJSON(c, p, 393216)
	case "mutate":
		p, e := s.Mutate(c.Request.Context(), key, actor, c.Request.Body)
		if e != nil {
			RespondError(c, e)
			return
		}
		writeJSON(c, p, 393216)
	case "prepare":
		p, e := s.Prepare(c.Request.Context(), key, actor, c.Request.Body)
		if e != nil {
			RespondError(c, e)
			return
		}
		writeJSON(c, p, 32768)
	case "self":
		p, e := s.Self(c.Request.Context(), key)
		if e != nil {
			RespondError(c, e)
			return
		}
		writeJSON(c, p, MaxBundleBytes)
	case "catalog":
		p, e := s.Get(c.Request.Context(), key, actor)
		if e != nil {
			RespondError(c, e)
			return
		}
		release, e := loadRelease(c.Request.Context(), s.Releases, p.Bundle.Pin())
		if e != nil {
			RespondError(c, e)
			return
		}
		c.Data(http.StatusOK, "application/json; charset=utf-8", release.Bytes)
	default:
		RespondError(c, ErrUnavailable)
	}
}
