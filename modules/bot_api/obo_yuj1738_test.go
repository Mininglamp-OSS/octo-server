// Package bot_api — YUJ-1738 / octo-server#131 R2 regression tests.
//
// Two unrelated R2 review blockers (Jerry-Xin) bundled into one PR:
//
//   B1. oboUpdateGrantReq.Active was `*int`, but octo-web's
//       PersonaSettings ships `{"active": false}` as a JSON boolean.
//       encoding/json's default decoder rejected the boolean token,
//       silently 400'd the entire PUT, and the persona toggle was
//       inert end-to-end. The fix replaces the type with
//       `*FlexBoolInt`, which UnmarshalJSON's both shapes
//       (true/false → 1/0, integer N → N).
//
//   B2. setGrantActive's UPDATE statements scoped only by `id=?`
//       could resurrect a tombstoned grant on the activate path —
//       a DELETE that committed between the handler's gate and
//       our tx start would re-run with active=1 / revoked_at=NULL,
//       silently un-deleting. The fix re-checks `revoked_at != nil`
//       inside the tx and adds `AND revoked_at IS NULL` to the
//       UPDATE WHERE clause as defense-in-depth. The pause path
//       gets the same guards, though its harm potential was
//       narrower (no column reset).
//
// Verification scenarios mirror the four cases the YUJ-1738 task
// description called out.
package bot_api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
)

// makeRawCtx — sibling to makeCtx that ships a pre-encoded JSON body
// instead of marshalling a Go value. The B1 tests need this so the
// wire-shape they assert (boolean vs. integer for `active`) is the
// shape the handler sees, not whatever encoding/json produces from a
// FlexBoolInt round-trip.
func makeRawCtx(t *testing.T, uid, method, path string, rawBody []byte, params gin.Params) (*wkhttp.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(method, path, bytes.NewReader(rawBody))
	req.Header.Set("Content-Type", "application/json")
	gc.Request = req
	gc.Params = params
	c := &wkhttp.Context{Context: gc}
	if uid != "" {
		c.Set("uid", uid)
	}
	return c, rec
}

// ============================================================
// B1 — JSON boolean / integer dual decode for `active`
// ============================================================

// TestOBO_YUJ1738_B1_BooleanFalse_DecodesAsPause — verification #1:
// raw body `{"active": false}` (JSON boolean) must decode and pause
// the grant. This is the exact wire shape octo-web's PersonaSettings
// ships; pre-fix it 400'd at BindJSON and the row never moved.
func TestOBO_YUJ1738_B1_BooleanFalse_DecodesAsPause(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tRESTOwner, tRESTBot, "auto", "")
	ba := newBAforREST(s)

	body := []byte(`{"active": false}`)
	c, rec := makeRawCtx(t, tRESTOwner, http.MethodPut,
		"/v1/obo/grants/"+strconv.FormatInt(gid, 10), body,
		gin.Params{{Key: "id", Value: strconv.FormatInt(gid, 10)}})
	ba.oboUpdateGrant(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT {active:false} must 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	g, _ := s.findGrantByID(gid)
	if g == nil || g.Active != 0 {
		t.Fatalf("expected paused (active=0), got %+v", g)
	}
	if g.RevokedAt != nil {
		t.Fatalf("paused row must keep revoked_at=NULL, got %v", g.RevokedAt)
	}
}

// TestOBO_YUJ1738_B1_BooleanTrue_DecodesAsActivate — verification #2:
// raw body `{"active": true}` (JSON boolean) must decode and
// reactivate a paused grant. Mirrors the YUJ-1735 reactivate test
// but exercises the boolean wire shape end-to-end.
func TestOBO_YUJ1738_B1_BooleanTrue_DecodesAsActivate(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tRESTOwner, tRESTBot, "auto", "")
	if err := s.setGrantActive(gid, 0); err != nil {
		t.Fatalf("setGrantActive(pause) failed: %v", err)
	}
	g0, _ := s.findGrantByID(gid)
	if g0 == nil || g0.Active != 0 || g0.RevokedAt != nil {
		t.Fatalf("setup: expected paused, got %+v", g0)
	}

	ba := newBAforREST(s)
	body := []byte(`{"active": true}`)
	c, rec := makeRawCtx(t, tRESTOwner, http.MethodPut,
		"/v1/obo/grants/"+strconv.FormatInt(gid, 10), body,
		gin.Params{{Key: "id", Value: strconv.FormatInt(gid, 10)}})
	ba.oboUpdateGrant(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT {active:true} must 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	g, _ := s.findGrantByID(gid)
	if g == nil || g.Active != 1 {
		t.Fatalf("expected active=1, got %+v", g)
	}
}

// TestOBO_YUJ1738_B1_NumericZero_DecodesAsPause — verification #3a:
// raw body `{"active": 0}` (JSON integer) must keep working — we
// must not regress the legacy numeric wire shape while adding
// boolean support.
func TestOBO_YUJ1738_B1_NumericZero_DecodesAsPause(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tRESTOwner, tRESTBot, "auto", "")
	ba := newBAforREST(s)

	body := []byte(`{"active": 0}`)
	c, rec := makeRawCtx(t, tRESTOwner, http.MethodPut,
		"/v1/obo/grants/"+strconv.FormatInt(gid, 10), body,
		gin.Params{{Key: "id", Value: strconv.FormatInt(gid, 10)}})
	ba.oboUpdateGrant(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT {active:0} must 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	g, _ := s.findGrantByID(gid)
	if g == nil || g.Active != 0 {
		t.Fatalf("expected paused (active=0), got %+v", g)
	}
}

// TestOBO_YUJ1738_B1_NumericOne_DecodesAsActivate — verification #3b:
// raw body `{"active": 1}` (JSON integer) must keep working as the
// activate signal.
func TestOBO_YUJ1738_B1_NumericOne_DecodesAsActivate(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tRESTOwner, tRESTBot, "auto", "")
	if err := s.setGrantActive(gid, 0); err != nil {
		t.Fatalf("setGrantActive(pause) failed: %v", err)
	}

	ba := newBAforREST(s)
	body := []byte(`{"active": 1}`)
	c, rec := makeRawCtx(t, tRESTOwner, http.MethodPut,
		"/v1/obo/grants/"+strconv.FormatInt(gid, 10), body,
		gin.Params{{Key: "id", Value: strconv.FormatInt(gid, 10)}})
	ba.oboUpdateGrant(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT {active:1} must 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	g, _ := s.findGrantByID(gid)
	if g == nil || g.Active != 1 {
		t.Fatalf("expected active=1, got %+v", g)
	}
}

// TestOBO_YUJ1738_B1_BadShape_400 — defensive: non-bool / non-int
// shapes (string, array, object, float) must still be rejected.
// FlexBoolInt's UnmarshalJSON returns a typed error which BindJSON
// surfaces as 400 — the legacy `*int` decoder produced the same
// failure mode for these shapes, so the regression posture matches.
func TestOBO_YUJ1738_B1_BadShape_400(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tRESTOwner, tRESTBot, "auto", "")
	ba := newBAforREST(s)

	for _, body := range [][]byte{
		[]byte(`{"active": "yes"}`),
		[]byte(`{"active": [1]}`),
		[]byte(`{"active": {"on":true}}`),
	} {
		c, rec := makeRawCtx(t, tRESTOwner, http.MethodPut,
			"/v1/obo/grants/"+strconv.FormatInt(gid, 10), body,
			gin.Params{{Key: "id", Value: strconv.FormatInt(gid, 10)}})
		ba.oboUpdateGrant(c)
		if rec.Code == http.StatusOK {
			t.Errorf("PUT %s must NOT 200, got 200 body=%s", string(body), rec.Body.String())
		}
		g, _ := s.findGrantByID(gid)
		if g == nil || g.Active != 1 {
			t.Errorf("rejected PUT %s must not mutate row, got %+v", string(body), g)
		}
	}
}

// TestOBO_YUJ1738_B1_OmittedField_NoOp — defensive: a body with no
// `active` field must still leave the row's active column untouched.
// This pins the "absent vs zero" pointer semantic that callers
// (including the no-op branch and the paused-vs-revoked gate) rely
// on. With FlexBoolInt as `int`-based, omission is the only way to
// get the nil pointer.
func TestOBO_YUJ1738_B1_OmittedField_NoOp(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tRESTOwner, tRESTBot, "auto", "")
	ba := newBAforREST(s)

	body := []byte(`{}`)
	c, rec := makeRawCtx(t, tRESTOwner, http.MethodPut,
		"/v1/obo/grants/"+strconv.FormatInt(gid, 10), body,
		gin.Params{{Key: "id", Value: strconv.FormatInt(gid, 10)}})
	ba.oboUpdateGrant(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty PUT must 200 (no-op), got %d body=%s", rec.Code, rec.Body.String())
	}
	g, _ := s.findGrantByID(gid)
	if g == nil || g.Active != 1 {
		t.Fatalf("no-op must keep active=1, got %+v", g)
	}
}

// TestOBO_YUJ1738_B1_FlexBoolIntUnmarshal_Direct — unit test on the
// type itself, independent of the handler. Pins each accepted token
// shape and the rejection of un-supported shapes.
func TestOBO_YUJ1738_B1_FlexBoolIntUnmarshal_Direct(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    int
		wantErr bool
	}{
		{"true", `true`, 1, false},
		{"false", `false`, 0, false},
		{"zero", `0`, 0, false},
		{"one", `1`, 1, false},
		{"large_int", `42`, 42, false},
		{"negative", `-1`, -1, false},
		{"null", `null`, 0, false}, // defensive: null leaves zero
		{"string", `"true"`, 0, true},
		{"array", `[true]`, 0, true},
		{"object", `{"v":1}`, 0, true},
		{"float", `1.5`, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var f FlexBoolInt
			err := f.UnmarshalJSON([]byte(tc.input))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("input %s: expected error, got nil (value=%d)", tc.input, int(f))
				}
				return
			}
			if err != nil {
				t.Fatalf("input %s: unexpected err: %v", tc.input, err)
			}
			if int(f) != tc.want {
				t.Fatalf("input %s: want %d, got %d", tc.input, tc.want, int(f))
			}
		})
	}
}

// TestOBO_YUJ1738_B1_FlexBoolInt_RoundTrip — Marshal emits an
// integer (NOT a boolean) for downstream API consumers that read
// the grant model back as a number. Two-way round-trip must
// preserve the value.
func TestOBO_YUJ1738_B1_FlexBoolInt_RoundTrip(t *testing.T) {
	for _, n := range []int{0, 1, 7} {
		v := FlexBoolInt(n)
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("Marshal(%d) err=%v", n, err)
		}
		if string(raw) != strconv.Itoa(n) {
			t.Fatalf("Marshal(%d) = %s, want %d", n, string(raw), n)
		}
		var back FlexBoolInt
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatalf("Unmarshal(%s) err=%v", string(raw), err)
		}
		if int(back) != n {
			t.Fatalf("round-trip %d -> %s -> %d", n, string(raw), int(back))
		}
	}
}

// ============================================================
// B2 — DELETE-vs-PUT race guard on setGrantActive
// ============================================================

// TestOBO_YUJ1738_B2_RevokedRow_ActivateNoOp — verification #4:
// directly call setGrantActive(id, 1) on a row whose revoked_at
// has been set by a prior revokeGrant. The fake mirrors prod:
// the row must NOT be resurrected (active stays 0, revoked_at
// stays non-NULL). Pre-fix this would flip active back to 1 and
// clear revoked_at, undoing the DELETE.
func TestOBO_YUJ1738_B2_RevokedRow_ActivateNoOp(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tRESTOwner, tRESTBot, "auto", "")
	if err := s.revokeGrant(gid); err != nil {
		t.Fatalf("revokeGrant: %v", err)
	}
	g0, _ := s.findGrantByID(gid)
	if g0 == nil || g0.RevokedAt == nil {
		t.Fatalf("setup: expected revoked, got %+v", g0)
	}
	revokedAt := *g0.RevokedAt

	if err := s.setGrantActive(gid, 1); err != nil {
		t.Fatalf("setGrantActive(activate) on revoked row err=%v", err)
	}

	g, _ := s.findGrantByID(gid)
	if g == nil {
		t.Fatalf("row must still exist")
	}
	if g.Active != 0 {
		t.Fatalf("revoked row must remain active=0, got active=%d", g.Active)
	}
	if g.RevokedAt == nil {
		t.Fatalf("revoked_at must NOT be cleared, got nil")
	}
	if !g.RevokedAt.Equal(revokedAt) {
		t.Fatalf("revoked_at must not be touched: was %v, now %v", revokedAt, *g.RevokedAt)
	}
}

// TestOBO_YUJ1738_B2_RevokedRow_PauseNoOp — defensive: pausing a
// revoked row is a no-op too. The active column is already 0 and
// revoked_at must not be disturbed (audit timestamp).
func TestOBO_YUJ1738_B2_RevokedRow_PauseNoOp(t *testing.T) {
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tRESTOwner, tRESTBot, "auto", "")
	if err := s.revokeGrant(gid); err != nil {
		t.Fatalf("revokeGrant: %v", err)
	}
	g0, _ := s.findGrantByID(gid)
	revokedAt := *g0.RevokedAt

	if err := s.setGrantActive(gid, 0); err != nil {
		t.Fatalf("setGrantActive(pause) on revoked row err=%v", err)
	}

	g, _ := s.findGrantByID(gid)
	if g == nil || g.Active != 0 {
		t.Fatalf("revoked row must stay active=0, got %+v", g)
	}
	if g.RevokedAt == nil || !g.RevokedAt.Equal(revokedAt) {
		t.Fatalf("revoked_at must be preserved exactly, was %v now %v", revokedAt, g.RevokedAt)
	}
}

// TestOBO_YUJ1738_B2_RevokedRow_DoesNotDemoteSiblings — the activate
// path's demote-others step must NOT fire when the activate itself
// is no-op'd by the revoked guard. Without this, a tombstoned row
// would bystander-demote unrelated siblings under the same grantor.
func TestOBO_YUJ1738_B2_RevokedRow_DoesNotDemoteSiblings(t *testing.T) {
	s := newFakeOBOStore()
	// Two active grants under the same grantor; revoke the first,
	// then call setGrantActive(1) on the revoked one. The OTHER
	// grant must remain active=1.
	gidA, _ := s.insertGrant(tRESTOwner, "bot_a", "auto", "")
	gidB, _ := s.insertGrant(tRESTOwner, "bot_b", "auto", "")
	if err := s.revokeGrant(gidA); err != nil {
		t.Fatalf("revokeGrant(A): %v", err)
	}

	if err := s.setGrantActive(gidA, 1); err != nil {
		t.Fatalf("setGrantActive(A, 1) err=%v", err)
	}

	a, _ := s.findGrantByID(gidA)
	b, _ := s.findGrantByID(gidB)
	if a == nil || a.Active != 0 || a.RevokedAt == nil {
		t.Fatalf("A must remain revoked, got %+v", a)
	}
	if b == nil || b.Active != 1 {
		t.Fatalf("B must remain untouched (active=1), got %+v", b)
	}
}

// TestOBO_YUJ1738_B2_HappyPathStillWorks — sanity guard: the race
// guard must not break the normal activate flow. An active row +
// activate call demotes a sibling exactly as before.
func TestOBO_YUJ1738_B2_HappyPathStillWorks(t *testing.T) {
	s := newFakeOBOStore()
	gidA, _ := s.insertGrant(tRESTOwner, "bot_a", "auto", "")
	gidB, _ := s.insertGrant(tRESTOwner, "bot_b", "auto", "")
	// Pause B so A is the lone active row, then activate B.
	if err := s.setGrantActive(gidB, 0); err != nil {
		t.Fatalf("setGrantActive(B, 0): %v", err)
	}

	if err := s.setGrantActive(gidB, 1); err != nil {
		t.Fatalf("setGrantActive(B, 1): %v", err)
	}

	a, _ := s.findGrantByID(gidA)
	b, _ := s.findGrantByID(gidB)
	if b == nil || b.Active != 1 {
		t.Fatalf("B must be active=1, got %+v", b)
	}
	if a == nil || a.Active != 0 {
		t.Fatalf("A must be demoted to active=0, got %+v", a)
	}
	// YUJ-1744 / PR#131 R4 — siblings demoted by the activate path are
	// PAUSED, not REVOKED. revoked_at must remain NULL so a later PUT
	// {active:1} on this row can flip it back through oboUpdateGrant's
	// RevokedAt-gate. (Pre-R4 this asserted RevokedAt != nil — that
	// expectation encoded the very bug R4 fixes.)
	if a.RevokedAt != nil {
		t.Errorf("demoted sibling must keep revoked_at=NULL, got %v", a.RevokedAt)
	}
	// Quiet linter when no branch references time.
	_ = time.Now
}
