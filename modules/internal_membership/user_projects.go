package internal_membership

import (
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"go.uber.org/zap"
)

// ---------- GET /v1/internal/projects ----------

// listUserProjects answers every activated project of one Space that the named
// uid holds an active seat in, with that uid's role.
//
// Contract:
//   - 200 — a bare JSON array (no envelope, no pagination metadata); `[]` when
//     nothing qualifies, never `null`. Filtering, activation and ordering belong
//     to projectpkg.UserProjectsInSpace.
//   - 400 err.shared.param.invalid — space_id or uid missing, empty, padded or
//     repeated.
//   - 401 err.shared.auth.token_invalid — token failure (middleware).
//   - 500 err.shared.internal — lookup failure; never an empty 200.
//
// The selector comes from the query string only. This surface has no session
// identity: uid is a token-authorized query target, and no header or context
// value may override either selector.
func (m *Module) listUserProjects(c *wkhttp.Context) {
	spaceID, ok := singleQueryValue(c, "space_id")
	if !ok {
		respondInvalidParam(c, "space_id")
		return
	}
	uid, ok := singleQueryValue(c, "uid")
	if !ok {
		respondInvalidParam(c, "uid")
		return
	}

	rows, err := m.store.UserProjects(c.Request.Context(), spaceID, uid)
	if err != nil {
		// Keep the reason in the log, not on the wire. The scope ids make the line
		// actionable without exposing query text or DSN.
		m.Error("list user projects: lookup failed",
			zap.Error(err), zap.String("space_id", spaceID), zap.String("uid", uid))
		respondInternal(c)
		return
	}

	c.Response(rows)
}

// singleQueryValue accepts the field only when it appears exactly once, with a
// non-empty value and no leading/trailing whitespace. A repeated parameter is
// rejected rather than resolved to one of its values.
func singleQueryValue(c *wkhttp.Context, field string) (string, bool) {
	values := c.QueryArray(field)
	if len(values) != 1 {
		return "", false
	}
	v := values[0]
	if v == "" || v != strings.TrimSpace(v) {
		return "", false
	}
	return v, true
}
