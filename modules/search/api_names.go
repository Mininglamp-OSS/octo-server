package search

import (
	"context"
	"errors"
	"net/http"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/Mininglamp-OSS/octo-server/pkg/log"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

type nameLoader func(context.Context, string, string, nameQuery) (nameCandidateSet, error)

func (s *Search) contacts(c *wkhttp.Context) {
	serveNameSearch(c, "contact", nameSQLProvider{session: s.ctx.DB()}.Load)
}
func (s *Search) groups(c *wkhttp.Context) {
	serveNameSearch(c, "group", nameSQLProvider{session: s.ctx.DB()}.Load)
}

func (s *Search) threads(c *wkhttp.Context) {
	serveNameSearch(c, "thread", nameSQLProvider{session: s.ctx.DB()}.Load)
}

func serveNameSearch(c *wkhttp.Context, kind string, load nameLoader) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1024*1024)
	var q nameQuery
	if err := c.BindJSON(&q); err != nil {
		respondSearchRequestInvalid(c, "")
		return
	}
	if field := q.validate(kind); field != "" {
		respondSearchRequestInvalid(c, field)
		return
	}
	// SpaceMiddleware validates header/query scope; require the typed JSON
	// context to agree. A null space cannot inherit an automatic client header.
	if q.space() != spacepkg.GetSpaceID(c) {
		respondSearchRequestInvalid(c, "space_id")
		return
	}
	uid := c.GetLoginUID()
	if uid == "" {
		httperr.ResponseErrorL(c, errcode.ErrSearchNameForbidden, nil, nil)
		return
	}
	page, err := (nameSearchService{load: load}).search(c.Request.Context(), uid, kind, q)
	if err != nil {
		switch {
		case errors.Is(err, errNameQueryChanged):
			httperr.ResponseErrorL(c, errcode.ErrSearchNameQueryChanged, nil, nil)
		case errors.Is(err, errNameCursorInvalid):
			respondSearchRequestInvalid(c, "cursor")
		case errors.Is(err, errNameScopeUnsupported):
			respondSearchRequestInvalid(c, "scope")
		case errors.Is(err, errNameForbidden):
			httperr.ResponseErrorL(c, errcode.ErrSearchNameForbidden, nil, nil)
		default:
			log.Error("Name search candidate query failed", zap.Error(err), zap.String("uid", uid), zap.String("kind", kind))
			httperr.ResponseErrorL(c, errcode.ErrSearchNameQueryFailed, nil, i18n.Details{})
		}
		return
	}
	c.Response(page)
}
