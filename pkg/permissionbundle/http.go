package permissionbundle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

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
	return explicitSpaceQuery(r, q)
}
func explicitSpaceQuery(r *http.Request, q url.Values) (string, error) {
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

type catalogSelectors struct {
	space, format, path string
	pin                 CatalogPin
}

func parseCatalogSelectors(r *http.Request, file bool) (catalogSelectors, error) {
	q, e := url.ParseQuery(r.URL.RawQuery)
	if e != nil {
		return catalogSelectors{}, ErrInvalid
	}
	space, e := explicitSpaceQuery(r, q)
	if e != nil {
		return catalogSelectors{}, e
	}
	if !file && len(q) == 1 {
		return catalogSelectors{space: space, format: CatalogArtifactV1}, nil
	}
	allowed := map[string]bool{"space_id": true, "format": true, "catalog_version": true, "catalog_digest": true}
	if file {
		allowed["path"] = true
	}
	if len(q) != len(allowed) || r.Header.Get("Content-Encoding") != "" {
		return catalogSelectors{}, ErrInvalid
	}
	for key, values := range q {
		if !allowed[key] || len(values) != 1 || values[0] == "" {
			return catalogSelectors{}, ErrInvalid
		}
	}
	if q.Get("format") != CatalogPackageV2 || !validHex(q.Get("catalog_digest"), 64) {
		return catalogSelectors{}, ErrVersion
	}
	path := q.Get("path")
	if file {
		if _, _, _, ok := packagePath(path); !ok {
			return catalogSelectors{}, ErrInvalid
		}
	}
	return catalogSelectors{space, CatalogPackageV2, path, CatalogPin{q.Get("catalog_version"), q.Get("catalog_digest")}}, nil
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
	var selection catalogSelectors
	var space string
	var e error
	if operation == "catalog" || operation == "catalog_file" {
		selection, e = parseCatalogSelectors(c.Request, operation == "catalog_file")
		space = selection.space
	} else {
		space, e = ExplicitSpace(c.Request)
	}
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
	case "catalog", "catalog_file":
		// One deadline starts before the fresh scope/policy read and is reused
		// through immutable byte selection; it is not restarted per file.
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		p, e := s.Get(ctx, key, actor)
		if e != nil {
			RespondError(c, e)
			return
		}
		format, e := catalogFormat(ctx, s.Releases, p.Bundle.Pin())
		if e != nil || format != selection.format || format == CatalogPackageV2 && selection.pin != p.Bundle.Pin() {
			RespondError(c, ErrVersion)
			return
		}
		if operation == "catalog_file" {
			reader, ok := s.Releases.(catalogFileReader)
			if !ok {
				RespondError(c, ErrUnavailable)
				return
			}
			b, e := reader.CatalogFile(ctx, selection.pin, selection.path)
			if e != nil {
				RespondError(c, e)
				return
			}
			kind, _, _, _ := packagePath(selection.path)
			media := "application/json; charset=utf-8"
			if kind == "blob" {
				media = "application/octet-stream"
			}
			c.Header("Content-Encoding", "identity")
			c.Data(http.StatusOK, media, b)
			return
		}
		release, e := loadRelease(ctx, s.Releases, p.Bundle.Pin())
		if e != nil {
			RespondError(c, e)
			return
		}
		if format == CatalogPackageV2 {
			c.Header("Content-Encoding", "identity")
		}
		c.Data(http.StatusOK, "application/json; charset=utf-8", release.Bytes)
	default:
		RespondError(c, ErrUnavailable)
	}
}
