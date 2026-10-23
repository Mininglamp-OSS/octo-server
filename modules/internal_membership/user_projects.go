package internal_membership

import (
	"net/url"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"go.uber.org/zap"
)

// ---------- GET /v1/internal/projects ----------

// listProjects selects either the user's Project memberships or bare Project
// metadata from the query keys. The two modes are mutually exclusive.
//
// Contract:
//   - 200 — a bare JSON array; [] when nothing qualifies, never null.
//   - 400 err.shared.param.invalid — malformed query, invalid space_id or uid,
//     conflicting selectors, or an empty/oversized project_ids batch.
//   - 401 err.shared.auth.token_invalid — token failure (middleware).
//   - 500 err.shared.internal — lookup failure; never an empty 200.
//
// Both selectors come from the query string only. This surface has no session
// identity: uid is a token-authorized query target, while project_ids names the
// metadata candidates and does not imply membership or resource authorization.
func (m *Module) listProjects(c *wkhttp.Context) {
	query, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil {
		respondInvalidParam(c, "")
		return
	}

	spaceID, ok := singleQueryValue(query, "space_id")
	if !ok {
		respondInvalidParam(c, "space_id")
		return
	}
	_, hasUID := query["uid"]
	_, hasProjectIDs := query["project_ids"]
	if hasUID == hasProjectIDs {
		respondInvalidParam(c, "uid|project_ids")
		return
	}

	if hasUID {
		uid, ok := singleQueryValue(query, "uid")
		if !ok {
			respondInvalidParam(c, "uid")
			return
		}
		rows, err := m.store.UserProjects(c.Request.Context(), spaceID, uid)
		if err != nil {
			m.Error("list user projects: lookup failed",
				zap.Error(err), zap.String("space_id", spaceID), zap.String("uid", uid))
			respondInternal(c)
			return
		}
		c.Response(rows)
		return
	}

	projectIDs, overLimit := parseIDList(query["project_ids"], maxBatchProjectIDs)
	if len(projectIDs) == 0 || overLimit {
		respondInvalidParam(c, "project_ids")
		return
	}
	rows, err := m.store.ProjectMetadata(c.Request.Context(), spaceID, projectIDs)
	if err != nil {
		m.logLookupFailure("project metadata", err, spaceID, len(projectIDs))
		respondInternal(c)
		return
	}
	c.Response(rows)
}

// singleQueryValue accepts the field only when it appears exactly once, with a
// non-empty value and no leading/trailing whitespace. A repeated parameter is
// rejected rather than resolved to one of its values.
func singleQueryValue(query url.Values, field string) (string, bool) {
	values := query[field]
	if len(values) != 1 {
		return "", false
	}
	v := values[0]
	if v == "" || v != strings.TrimSpace(v) {
		return "", false
	}
	return v, true
}
