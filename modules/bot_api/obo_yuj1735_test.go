// Package bot_api — YUJ-1735 / octo-server#131 follow-up regression tests
// for the paused-vs-revoked active-gate split in oboUpdateGrant.
//
// Pre-fix, the active-gate (`if grant.Active != 1`) ran BEFORE BindJSON,
// so a PUT {"active": 1} on a paused grant (active=0, revoked_at=NULL)
// 404'd immediately and the persona selector in octo-web could not
// re-activate a previously-paused persona. Jerry-Xin and lml2468
// flagged this as a P0 blocker on the PR#131 review. The fix:
//
//  1. BindJSON moved BEFORE the gate.
//  2. Gate split into:
//     - revoked_at != NULL → 404 always.
//     - active == 0 && revoked_at == NULL && req.Active == nil → 404
//       (paused row, no reactivation intent in the body).
//     - otherwise → allow.
//
// These tests pin all four verification cases enumerated in the
// YUJ-1735 task description.
package bot_api

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestOBO_YUJ1735_Pause_OK — verification #1: PUT {active:0} on an
// active grant must succeed and flip the row to active=0 with
// revoked_at unchanged (NULL). Regression guard against the gate
// accidentally rejecting the pause path itself.
func TestOBO_YUJ1735_Pause_OK(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tRESTOwner, tRESTBot, "auto", "")
	ba := newBAforREST(s)

	zero := 0
	c, rec := makeCtx(t, tRESTOwner, http.MethodPut,
		"/v1/obo/grants/"+strconv.FormatInt(gid, 10),
		oboUpdateGrantReq{Active: flexPtr(zero)},
		gin.Params{{Key: "id", Value: strconv.FormatInt(gid, 10)}})
	ba.oboUpdateGrant(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("pause status=%d body=%s", rec.Code, rec.Body.String())
	}
	g, _ := s.findGrantByID(gid)
	if g == nil || g.Active != 0 {
		t.Fatalf("expected paused row active=0, got %+v", g)
	}
	if g.RevokedAt != nil {
		t.Fatalf("paused row must keep revoked_at=NULL (paused != revoked), got %v", g.RevokedAt)
	}
}

// TestOBO_YUJ1735_ReactivatePausedGrant — verification #2 (the actual
// blocker): PUT {active:1} on a paused grant (active=0,
// revoked_at=NULL) must succeed. Pre-fix this returned 404; post-fix
// the row flips back to active=1 and (since the activate path mirrors
// createOrReactivateGrantAtomic) revoked_at is cleared on demoted
// siblings rather than on the target itself, which already had
// revoked_at=NULL.
func TestOBO_YUJ1735_ReactivatePausedGrant(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tRESTOwner, tRESTBot, "auto", "")
	zero := 0
	if err := s.setGrantActive(gid, zero); err != nil {
		t.Fatalf("setGrantActive(pause) failed: %v", err)
	}
	g0, _ := s.findGrantByID(gid)
	if g0 == nil || g0.Active != 0 || g0.RevokedAt != nil {
		t.Fatalf("setup: expected paused (active=0, revoked_at=NULL), got %+v", g0)
	}

	ba := newBAforREST(s)
	one := 1
	c, rec := makeCtx(t, tRESTOwner, http.MethodPut,
		"/v1/obo/grants/"+strconv.FormatInt(gid, 10),
		oboUpdateGrantReq{Active: flexPtr(one)},
		gin.Params{{Key: "id", Value: strconv.FormatInt(gid, 10)}})
	ba.oboUpdateGrant(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("reactivate paused grant must 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	g, _ := s.findGrantByID(gid)
	if g == nil || g.Active != 1 {
		t.Fatalf("expected reactivated row active=1, got %+v", g)
	}
	if g.RevokedAt != nil {
		t.Fatalf("reactivated row must have revoked_at=NULL, got %v", g.RevokedAt)
	}
}

// TestOBO_YUJ1735_RevokedGrant_StillRejected — verification #3: a
// DELETE-revoked grant (active=0, revoked_at!=NULL) must still 404 on
// PUT {active:1}. The active field is NOT a back-door re-activation
// channel for tombstoned rows; revival of a revoked grant requires
// POST /v1/obo/grants. Mirrors the YUJ-1728 test but with the new
// gate semantics.
func TestOBO_YUJ1735_RevokedGrant_StillRejected(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tRESTOwner, tRESTBot, "auto", "")
	if err := s.revokeGrant(gid); err != nil {
		t.Fatalf("revokeGrant failed: %v", err)
	}
	g0, _ := s.findGrantByID(gid)
	if g0 == nil || g0.Active != 0 || g0.RevokedAt == nil {
		t.Fatalf("setup: expected revoked (active=0, revoked_at!=NULL), got %+v", g0)
	}

	ba := newBAforREST(s)
	one := 1
	c, rec := makeCtx(t, tRESTOwner, http.MethodPut,
		"/v1/obo/grants/"+strconv.FormatInt(gid, 10),
		oboUpdateGrantReq{Active: flexPtr(one)},
		gin.Params{{Key: "id", Value: strconv.FormatInt(gid, 10)}})
	ba.oboUpdateGrant(c)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("revoked-grant PUT {active:1} must 404, got %d body=%s",
			rec.Code, rec.Body.String())
	}
	g, _ := s.findGrantByID(gid)
	if g == nil || g.Active != 0 {
		t.Fatalf("revoked row must remain active=0, got %+v", g)
	}
	if g.RevokedAt == nil {
		t.Fatalf("revoked row must keep revoked_at!=NULL, got nil")
	}
}

// TestOBO_YUJ1735_PausedGrant_PutWithoutActive_404 — verification #4:
// PUT {global_enabled:1} (no `active` field) on a paused grant must
// still 404. The carve-out for paused rows is narrowly scoped to
// reactivation intent; settings-only PUTs on a paused row would land
// on a row no findActiveGrant* lookup will surface, reproducing the
// misleading-UX bug the original gate was added for.
func TestOBO_YUJ1735_PausedGrant_PutWithoutActive_404(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tRESTOwner, tRESTBot, "auto", "")
	zero := 0
	if err := s.setGrantActive(gid, zero); err != nil {
		t.Fatalf("setGrantActive(pause) failed: %v", err)
	}

	ba := newBAforREST(s)
	one := 1
	c, rec := makeCtx(t, tRESTOwner, http.MethodPut,
		"/v1/obo/grants/"+strconv.FormatInt(gid, 10),
		oboUpdateGrantReq{GlobalEnabled: &one}, // no Active field
		gin.Params{{Key: "id", Value: strconv.FormatInt(gid, 10)}})
	ba.oboUpdateGrant(c)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("paused-grant PUT without active field must 404, got %d body=%s",
			rec.Code, rec.Body.String())
	}
	g, _ := s.findGrantByID(gid)
	if g == nil || g.Active != 0 || g.GlobalEnabled == 1 {
		t.Fatalf("rejected PUT must not mutate row, got %+v", g)
	}
}

// TestOBO_YUJ1735_PausedGrant_RepauseIdempotent — bonus: PUT {active:0}
// on an already-paused grant is allowed (req.Active != nil satisfies
// the carve-out) and is a logical no-op. Documents the edge case so a
// future tightening of the gate doesn't accidentally regress it.
func TestOBO_YUJ1735_PausedGrant_RepauseIdempotent(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tRESTOwner, tRESTBot, "auto", "")
	zero := 0
	if err := s.setGrantActive(gid, zero); err != nil {
		t.Fatalf("setGrantActive(pause) failed: %v", err)
	}

	ba := newBAforREST(s)
	c, rec := makeCtx(t, tRESTOwner, http.MethodPut,
		"/v1/obo/grants/"+strconv.FormatInt(gid, 10),
		oboUpdateGrantReq{Active: flexPtr(zero)},
		gin.Params{{Key: "id", Value: strconv.FormatInt(gid, 10)}})
	ba.oboUpdateGrant(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-pause idempotent PUT must 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	g, _ := s.findGrantByID(gid)
	if g == nil || g.Active != 0 || g.RevokedAt != nil {
		t.Fatalf("re-pause must keep paused (active=0, revoked_at=NULL), got %+v", g)
	}
	// Quiet the unused-import linter for `time` even when no branch
	// references it directly; pause path does not stamp revoked_at,
	// but the test asserts the absence of any timestamp drift.
	_ = time.Now
}
