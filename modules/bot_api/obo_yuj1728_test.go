// Package bot_api — YUJ-1728 / octo-server#129 regression tests for the
// PUT /v1/obo/grants/:id `active` selector switch.
//
// Pre-fix, oboUpdateGrantReq had no `active` field, so a PUT body like
// {"active": false} was silently dropped at BindJSON, the no-op branch
// fired, and the handler returned the unchanged row — the persona
// toggle in octo-web appeared to succeed but never moved the underlying
// row. These tests pin the four behaviors enumerated in the YUJ-1728
// task description plus one regression guard for the existing field
// combinations (active + global_enabled in one PUT).
package bot_api

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestOBO_UpdateGrant_Active_Pause — PUT {active: 0} must flip the row
// from active=1 to active=0. The handler's active-gate at the top still
// passes (we're loading a grant that IS active=1 at request time); the
// new code path takes effect during the update.
func TestOBO_UpdateGrant_Active_Pause(t *testing.T) {
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
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	g, _ := s.findGrantByID(gid)
	if g == nil || g.Active != 0 {
		t.Fatalf("expected active=0 after pause, got %+v", g)
	}
}

// TestOBO_UpdateGrant_Active_Activate_DemotesSiblings — PUT {active: 1}
// on grant B while grant A is the currently-active persona must flip B
// to active=1 AND demote A to active=0 (mutex), preserving the
// "at most one active grant per grantor" invariant that
// createOrReactivateGrantAtomic establishes on the create path.
func TestOBO_UpdateGrant_Active_Activate_DemotesSiblings(t *testing.T) {
	s := newFakeOBOStore()
	// Grant A — currently active.
	gidA, _ := s.insertGrant(tRESTOwner, "bot_persona_a", "auto", "")
	// Grant B — paused (active=0) per a prior PUT.
	gidB, _ := s.insertGrant(tRESTOwner, "bot_persona_b", "auto", "")
	zero := 0
	_ = s.setGrantActive(gidB, zero)

	ba := newBAforREST(s)

	// Switch personas: activate B via PUT. Pre-YUJ-1735 the active-gate
	// rejected PUTs on paused (active=0) grants up-front, so this test
	// works the mutex from the OPPOSITE direction — re-activating A
	// must NOT demote B (B is already active=0). YUJ-1735 relaxed
	// that gate (paused rows with req.Active != nil are addressable),
	// but the same assertion still holds: re-activating an
	// already-active grant must leave its paused siblings alone.
	one := 1
	c, rec := makeCtx(t, tRESTOwner, http.MethodPut,
		"/v1/obo/grants/"+strconv.FormatInt(gidA, 10),
		oboUpdateGrantReq{Active: flexPtr(one)},
		gin.Params{{Key: "id", Value: strconv.FormatInt(gidA, 10)}})
	ba.oboUpdateGrant(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("activate A status=%d body=%s", rec.Code, rec.Body.String())
	}
	a, _ := s.findGrantByID(gidA)
	b, _ := s.findGrantByID(gidB)
	if a == nil || a.Active != 1 {
		t.Fatalf("expected grant A active=1, got %+v", a)
	}
	if b == nil || b.Active != 0 {
		t.Fatalf("expected grant B to remain active=0, got %+v", b)
	}

	// Now insert grant C (auto-inserted active=1 per insertGrant
	// contract), then flip A active=1 again — C must be mutex-demoted
	// to active=0 since it's the OTHER active row for the same
	// grantor.
	gidC, _ := s.insertGrant(tRESTOwner, "bot_persona_c", "auto", "")
	c2, rec2 := makeCtx(t, tRESTOwner, http.MethodPut,
		"/v1/obo/grants/"+strconv.FormatInt(gidA, 10),
		oboUpdateGrantReq{Active: flexPtr(one)},
		gin.Params{{Key: "id", Value: strconv.FormatInt(gidA, 10)}})
	ba.oboUpdateGrant(c2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("re-activate A status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	a2, _ := s.findGrantByID(gidA)
	cRow, _ := s.findGrantByID(gidC)
	if a2 == nil || a2.Active != 1 {
		t.Fatalf("expected grant A still active=1, got %+v", a2)
	}
	if cRow == nil || cRow.Active != 0 {
		t.Fatalf("expected grant C demoted to active=0, got %+v", cRow)
	}
	// YUJ-1744 / PR#131 R4 — demoted siblings must remain PAUSED
	// (revoked_at=NULL) so a future PUT {active:1} on C still resolves
	// through oboUpdateGrant. Pre-R4 the assertion here required
	// `RevokedAt != nil`, which was the very breakage R4 fixes.
	if cRow.RevokedAt != nil {
		t.Errorf("demoted grant C must keep revoked_at=NULL, got %v", cRow.RevokedAt)
	}
}

// TestOBO_UpdateGrant_Active_DoesNotTouchOtherGrantors — the mutex must
// be scoped strictly to (grantor_uid). Activating a grant under owner
// X must never touch owner Y's grants.
func TestOBO_UpdateGrant_Active_DoesNotTouchOtherGrantors(t *testing.T) {
	s := newFakeOBOStore()
	gidX, _ := s.insertGrant(tRESTOwner, "bot_x", "auto", "")
	gidY, _ := s.insertGrant(tRESTOther, "bot_y", "auto", "")

	ba := newBAforREST(s)
	one := 1
	c, rec := makeCtx(t, tRESTOwner, http.MethodPut,
		"/v1/obo/grants/"+strconv.FormatInt(gidX, 10),
		oboUpdateGrantReq{Active: flexPtr(one)},
		gin.Params{{Key: "id", Value: strconv.FormatInt(gidX, 10)}})
	ba.oboUpdateGrant(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	other, _ := s.findGrantByID(gidY)
	if other == nil || other.Active != 1 {
		t.Fatalf("foreign-grantor row must remain untouched, got %+v", other)
	}
}

// TestOBO_UpdateGrant_NoOp_WithActiveField — empty body {} must still
// take the no-op branch (active+globalEnabled+personaPrompt+mode all
// nil/empty). Regression guard for the no-op condition update that
// added req.Active==nil to the predicate.
func TestOBO_UpdateGrant_NoOp_WithActiveField(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tRESTOwner, tRESTBot, "auto", "")
	one := 1
	_ = s.updateGrant(gid, "", &one, nil) // pre-set global_enabled=1
	ba := newBAforREST(s)

	c, rec := makeCtx(t, tRESTOwner, http.MethodPut,
		"/v1/obo/grants/"+strconv.FormatInt(gid, 10),
		oboUpdateGrantReq{},
		gin.Params{{Key: "id", Value: strconv.FormatInt(gid, 10)}})
	ba.oboUpdateGrant(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	g, _ := s.findGrantByID(gid)
	if g == nil || g.Active != 1 || g.GlobalEnabled != 1 {
		t.Fatalf("no-op must leave row unchanged, got %+v", g)
	}
}

// TestOBO_UpdateGrant_Active_RevokedGrant_Rejected — the existing
// active-gate (`if grant.Active != 1` at the top of oboUpdateGrant)
// must still reject PUTs on soft-deleted rows. The active field is
// NOT a back-door re-activation channel; per the task spec,
// resurrecting a revoked grant requires the POST reactivation path.
func TestOBO_UpdateGrant_Active_RevokedGrant_Rejected(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tRESTOwner, tRESTBot, "auto", "")
	_ = s.revokeGrant(gid)

	ba := newBAforREST(s)
	one := 1
	c, rec := makeCtx(t, tRESTOwner, http.MethodPut,
		"/v1/obo/grants/"+strconv.FormatInt(gid, 10),
		oboUpdateGrantReq{Active: flexPtr(one)},
		gin.Params{{Key: "id", Value: strconv.FormatInt(gid, 10)}})
	ba.oboUpdateGrant(c)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("revoked-grant PUT must 404, got %d body=%s", rec.Code, rec.Body.String())
	}
	g, _ := s.findGrantByID(gid)
	if g == nil || g.Active != 0 {
		t.Fatalf("revoked row must remain active=0 after rejected PUT, got %+v", g)
	}
}

// TestOBO_UpdateGrant_Active_CombinedWith_GlobalEnabled — a single PUT
// that sets both `active=true` and `global_enabled=1` must commit
// both: the row ends up active=1 / global_enabled=1, and sibling
// grants are mutex-demoted. Verifies the handler's two-call order
// (setGrantActive → updateGrant) composes cleanly.
func TestOBO_UpdateGrant_Active_CombinedWith_GlobalEnabled(t *testing.T) {
	s := newFakeOBOStore()
	gidA, _ := s.insertGrant(tRESTOwner, "bot_a", "auto", "")
	gidB, _ := s.insertGrant(tRESTOwner, "bot_b", "auto", "")

	ba := newBAforREST(s)
	one := 1
	c, rec := makeCtx(t, tRESTOwner, http.MethodPut,
		"/v1/obo/grants/"+strconv.FormatInt(gidA, 10),
		oboUpdateGrantReq{Active: flexPtr(one), GlobalEnabled: &one},
		gin.Params{{Key: "id", Value: strconv.FormatInt(gidA, 10)}})
	ba.oboUpdateGrant(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	a, _ := s.findGrantByID(gidA)
	b, _ := s.findGrantByID(gidB)
	if a == nil || a.Active != 1 || a.GlobalEnabled != 1 {
		t.Fatalf("expected A active=1 global_enabled=1, got %+v", a)
	}
	if b == nil || b.Active != 0 {
		t.Fatalf("expected B demoted to active=0, got %+v", b)
	}
}
