// Package bot_api — YUJ-1744 / octo-server#131 R4 regression tests.
//
// Bug: the sibling-demotion UPDATE in setGrantActive (and, by the
// same flaw, createOrReactivateGrantAtomic) stamped `revoked_at=now`
// on rows it demoted. oboUpdateGrant's revoked-row gate then 404'd
// any future PUT {active:1} on those rows, turning the persona
// selector into a one-way trip:
//
//   A active → switch to B (PUT {active:1} on B) → A demoted with
//   revoked_at=now. Switch back to A → 404.
//
// Fix: siblings are PAUSED, not REVOKED. Only revokeGrant (DELETE
// /v1/obo/grants/:id) writes `revoked_at`. The demote UPDATE drops
// the `revoked_at` column from its SET clause and adds `AND
// revoked_at IS NULL` to its WHERE clause as belt-and-braces against
// disturbing a row a concurrent DELETE just tombstoned.
//
// The three verification cases mirror the YUJ-1744 task description.
package bot_api

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestOBO_YUJ1744_SwitchAtoB_LeavesARevokedAtNULL — verification #1.
// Toggling from persona A to persona B via PUT {active:1} on B must
// flip A to active=0 with revoked_at unchanged (NULL). Pre-fix this
// stamped revoked_at=now on A.
func TestOBO_YUJ1744_SwitchAtoB_LeavesARevokedAtNULL(t *testing.T) {
	s := newFakeOBOStore()
	gidA, _ := s.insertGrant(tRESTOwner, "bot_persona_a", "auto", "")
	gidB, _ := s.insertGrant(tRESTOwner, "bot_persona_b", "auto", "")
	// insertGrant lands rows active=1; pause B so the setup mirrors
	// the real "A is the currently-selected persona" state.
	if err := s.setGrantActive(gidB, 0); err != nil {
		t.Fatalf("setup: pause B: %v", err)
	}
	a0, _ := s.findGrantByID(gidA)
	if a0 == nil || a0.Active != 1 || a0.RevokedAt != nil {
		t.Fatalf("setup: expected A active=1, revoked_at=NULL, got %+v", a0)
	}

	// Switch to B.
	ba := newBAforREST(s)
	one := 1
	c, rec := makeCtx(t, tRESTOwner, http.MethodPut,
		"/v1/obo/grants/"+strconv.FormatInt(gidB, 10),
		oboUpdateGrantReq{Active: flexPtr(one)},
		gin.Params{{Key: "id", Value: strconv.FormatInt(gidB, 10)}})
	ba.oboUpdateGrant(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("switch A→B status=%d body=%s", rec.Code, rec.Body.String())
	}

	a, _ := s.findGrantByID(gidA)
	if a == nil {
		t.Fatalf("A vanished after switch")
	}
	if a.Active != 0 {
		t.Fatalf("A must be demoted to active=0, got %d", a.Active)
	}
	if a.RevokedAt != nil {
		t.Fatalf("A.revoked_at must remain NULL after demotion (paused != revoked), got %v", a.RevokedAt)
	}
}

// TestOBO_YUJ1744_SwitchBackToA — verification #2: after switching
// A→B, a subsequent PUT {active:1} on A must succeed (200) and flip
// A back to active=1, demoting B. Pre-fix this returned 404 because
// A.revoked_at had been stamped during the A→B switch.
func TestOBO_YUJ1744_SwitchBackToA(t *testing.T) {
	s := newFakeOBOStore()
	gidA, _ := s.insertGrant(tRESTOwner, "bot_persona_a", "auto", "")
	gidB, _ := s.insertGrant(tRESTOwner, "bot_persona_b", "auto", "")
	if err := s.setGrantActive(gidB, 0); err != nil {
		t.Fatalf("setup: pause B: %v", err)
	}

	ba := newBAforREST(s)
	one := 1

	// Switch A → B.
	c1, rec1 := makeCtx(t, tRESTOwner, http.MethodPut,
		"/v1/obo/grants/"+strconv.FormatInt(gidB, 10),
		oboUpdateGrantReq{Active: flexPtr(one)},
		gin.Params{{Key: "id", Value: strconv.FormatInt(gidB, 10)}})
	ba.oboUpdateGrant(c1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("A→B switch: status=%d body=%s", rec1.Code, rec1.Body.String())
	}

	// Switch B → A. This is the operation that pre-R4 404'd.
	c2, rec2 := makeCtx(t, tRESTOwner, http.MethodPut,
		"/v1/obo/grants/"+strconv.FormatInt(gidA, 10),
		oboUpdateGrantReq{Active: flexPtr(one)},
		gin.Params{{Key: "id", Value: strconv.FormatInt(gidA, 10)}})
	ba.oboUpdateGrant(c2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("B→A switch must succeed (was 404 pre-R4): status=%d body=%s",
			rec2.Code, rec2.Body.String())
	}

	a, _ := s.findGrantByID(gidA)
	b, _ := s.findGrantByID(gidB)
	if a == nil || a.Active != 1 {
		t.Fatalf("A must be reactivated (active=1), got %+v", a)
	}
	if a.RevokedAt != nil {
		t.Fatalf("A.revoked_at must remain NULL on reactivation, got %v", a.RevokedAt)
	}
	if b == nil || b.Active != 0 {
		t.Fatalf("B must be demoted back to active=0, got %+v", b)
	}
	if b.RevokedAt != nil {
		t.Fatalf("B.revoked_at must remain NULL after demotion, got %v", b.RevokedAt)
	}
}

// TestOBO_YUJ1744_DeletedA_DemoteDoesNotTouchRevokedAt — verification
// #3: after DELETE A (revokeGrant), A.revoked_at is non-NULL. A
// subsequent persona switch (PUT {active:1} on B) must NOT touch A's
// revoked_at timestamp — the demote WHERE is scoped to
// `revoked_at IS NULL` so a tombstoned row keeps its audit stamp
// exactly, even when it happens to still have active=1 in some
// pathological race window.
func TestOBO_YUJ1744_DeletedA_DemoteDoesNotTouchRevokedAt(t *testing.T) {
	s := newFakeOBOStore()
	gidA, _ := s.insertGrant(tRESTOwner, "bot_persona_a", "auto", "")
	gidB, _ := s.insertGrant(tRESTOwner, "bot_persona_b", "auto", "")
	if err := s.setGrantActive(gidB, 0); err != nil {
		t.Fatalf("setup: pause B: %v", err)
	}

	// DELETE A — sets revoked_at, flips active=0.
	if err := s.revokeGrant(gidA); err != nil {
		t.Fatalf("revokeGrant(A): %v", err)
	}
	a0, _ := s.findGrantByID(gidA)
	if a0 == nil || a0.RevokedAt == nil {
		t.Fatalf("setup: A must be revoked, got %+v", a0)
	}
	revokedAt := *a0.RevokedAt

	// Switch to B. Demote-others must skip A (already inactive +
	// revoked) without disturbing its audit timestamp.
	ba := newBAforREST(s)
	one := 1
	c, rec := makeCtx(t, tRESTOwner, http.MethodPut,
		"/v1/obo/grants/"+strconv.FormatInt(gidB, 10),
		oboUpdateGrantReq{Active: flexPtr(one)},
		gin.Params{{Key: "id", Value: strconv.FormatInt(gidB, 10)}})
	ba.oboUpdateGrant(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("activate B status=%d body=%s", rec.Code, rec.Body.String())
	}

	a, _ := s.findGrantByID(gidA)
	if a == nil {
		t.Fatalf("A vanished")
	}
	if a.Active != 0 {
		t.Fatalf("A must stay active=0, got %d", a.Active)
	}
	if a.RevokedAt == nil {
		t.Fatalf("A.revoked_at must be preserved, got nil")
	}
	if !a.RevokedAt.Equal(revokedAt) {
		t.Fatalf("A.revoked_at must be byte-identical to pre-switch value: was %v, now %v",
			revokedAt, *a.RevokedAt)
	}
}
