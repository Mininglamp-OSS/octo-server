package project

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// auditRecorder collects audit entries through the injectable sink.
type auditRecorder struct {
	mu      sync.Mutex
	entries []AuditEntry
}

func (r *auditRecorder) sink(e AuditEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, e)
}

func (r *auditRecorder) byAction(action string) []AuditEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []AuditEntry
	for _, e := range r.entries {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// TestEveryWritePathEmitsAnAuditEntry covers create / disband / member add / member remove
// / role change, each carrying the actor, the target and (where the action has one) the
// reason.
func TestEveryWritePathEmitsAnAuditEntry(t *testing.T) {
	_, p := setup(t)
	rec := &auditRecorder{}
	p.auditSink = rec.sink
	r := mountProject(t, p)

	seedSpace(t, spaceA, 1)
	ownerTok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	memberTok := seedUser(t, "m1")
	seedSpaceMember(t, spaceA, "m1", 0, 1)
	seedUser(t, "m2")
	seedSpaceMember(t, spaceA, "m2", 0, 1)

	w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", ownerTok,
		map[string]any{"name": "audited"})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	created := decodeResp(t, w)

	w = doOn(t, r, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add", ownerTok,
		map[string]any{"uids": []string{"m1", "m2"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	w = doOn(t, r, http.MethodPut, "/v1/projects/"+created.ProjectID+"/members/m1/role", ownerTok,
		map[string]any{"role": RoleAdmin})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	w = doOn(t, r, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/remove", ownerTok,
		map[string]any{"uids": []string{"m2"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	w = doOn(t, r, http.MethodPost, "/v1/projects/"+created.ProjectID+"/leave", memberTok, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	w = doOn(t, r, http.MethodPut, "/v1/projects/"+created.ProjectID, ownerTok,
		map[string]any{"name": "audited2"})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	w = doOn(t, r, http.MethodDelete, "/v1/projects/"+created.ProjectID, ownerTok, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	for _, action := range []string{
		auditCreate, auditUpdate, auditDisband, auditMemberAdd, auditMemberRemove,
		auditLeave, auditRoleChange,
	} {
		entries := rec.byAction(action)
		require.NotEmpty(t, entries, "no audit entry for %s", action)
		for _, e := range entries {
			assert.NotEmpty(t, e.ActorUID, "%s must record the actor", action)
			assert.Equal(t, created.ProjectID, e.ProjectID, "%s must record the project", action)
			assert.Equal(t, spaceA, e.SpaceID, "%s must record the space", action)
		}
	}

	// Actions about a specific person must name them.
	for _, action := range []string{auditMemberAdd, auditMemberRemove, auditLeave, auditRoleChange} {
		for _, e := range rec.byAction(action) {
			assert.NotEmpty(t, e.TargetUID, "%s must record the target", action)
		}
	}
	// And the removal actions carry a reason.
	for _, e := range rec.byAction(auditMemberRemove) {
		assert.Equal(t, "kicked", e.Reason)
	}
	for _, e := range rec.byAction(auditLeave) {
		assert.Equal(t, "left", e.Reason)
	}
}

// TestCascadeEmitsAnAuditEntry covers the background path, which the HTTP cases cannot
// reach.
func TestCascadeEmitsAnAuditEntry(t *testing.T) {
	srv, p := setup(t)
	_, _, created := projectWithMembers(t, srv, "leaver")

	rec := &auditRecorder{}
	p.auditSink = rec.sink
	removeSpaceMember(t, spaceA, "leaver")
	require.NoError(t, runCascade(t, p, spaceA, "leaver", "owner1", "kicked"))

	entries := rec.byAction(auditCascade)
	require.Len(t, entries, 1)
	assert.Equal(t, "leaver", entries[0].TargetUID)
	assert.Equal(t, "owner1", entries[0].ActorUID)
	assert.Equal(t, created.ProjectID, entries[0].ProjectID)
	assert.Equal(t, "kicked", entries[0].Reason)
}

// TestWriteRejectionIsBrokenDownByEntryPoint pins the metric SHAPE, not a number.
//
// The breakdown is the whole point: P1 adds several more membership write paths, and a
// single undifferentiated counter cannot tell you that one of them skipped invariant I1.
// A test that only checked "the counter went up" would pass against exactly the metric the
// acceptance criterion rules out.
func TestWriteRejectionIsBrokenDownByEntryPoint(t *testing.T) {
	srv, _ := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv)
	seedUser(t, "outsider")

	before := testutil.ToFloat64(writeRejected.WithLabelValues(entryMemberAdd, reasonNotSpaceMember))
	otherBefore := testutil.ToFloat64(writeRejected.WithLabelValues(entryRoleChange, reasonNotSpaceMember))

	w := doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add",
		ownerTok, map[string]any{"uids": []string{"outsider"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	after := testutil.ToFloat64(writeRejected.WithLabelValues(entryMemberAdd, reasonNotSpaceMember))
	assert.Equal(t, before+1, after, "the rejection must be counted against member_add")
	assert.Equal(t, otherBefore,
		testutil.ToFloat64(writeRejected.WithLabelValues(entryRoleChange, reasonNotSpaceMember)),
		"and must NOT be counted against another entry point")
}

// TestCacheKeysStayInTheProjectNamespace is the runtime half of the namespace check: after
// a real membership write the only new key this module minted is under project:, and the
// Space fact lives under the key modules/space owns and invalidates.
func TestCacheKeysStayInTheProjectNamespace(t *testing.T) {
	srv, _ := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv, "m1")

	// A read warms both caches.
	w := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/members", ownerTok, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	keys := redisKeys(t, "project:*")
	require.NotEmpty(t, keys, "the project membership cache should have been populated")
	for _, k := range keys {
		assert.True(t, len(k) > len("project:member:") && k[:len("project:member:")] == "project:member:",
			"unexpected project-namespaced key %q", k)
	}
	// The Space membership fact is under space:member:*, which modules/space deletes
	// synchronously on removal — reusing it is a correctness requirement, not a naming
	// accident.
	assert.NotEmpty(t, redisKeys(t, "space:member:*"),
		"the Space gate must populate the key modules/space invalidates")
	// And nothing landed in the rate-limit namespace by accident.
	for _, k := range keys {
		assert.NotContains(t, k, "ratelimit:")
	}
}

// TestSpaceAdminDoesNotSeeUnlistedProjectsInTheUserList pins the P0 scope boundary.
//
// The brief grants a Space admin the real payload on the DETAIL route, and scopes "a Space admin
// can still enumerate project metadata" to the P2 admin surface. So the P0 user-facing list must
// NOT widen for them: an earlier version did, which shipped a slice of that P2 capability early
// and made "unlisted" mean nothing on the one route users actually read.
//
// Detail access is asserted separately (TestUnlistedProjectIsIndistinguishableFromNonexistent
// covers the non-member case); here the point is only that the list does not enumerate.
func TestSpaceAdminDoesNotSeeUnlistedProjectsInTheUserList(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	ownerTok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	adminTok := seedUser(t, "spaceadmin")
	seedSpaceMember(t, spaceA, "spaceadmin", 1, 1) // role 1 = Space admin

	hidden := createProjectVia(t, srv, spaceA, ownerTok, "hidden")
	w := doJSON(t, srv, http.MethodPut, "/v1/projects/"+hidden.ProjectID, ownerTok,
		map[string]any{"discoverability": DiscoverabilityUnlisted})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	w = doJSON(t, srv, http.MethodGet, "/v1/space/"+spaceA+"/projects", adminTok, nil)
	require.Equal(t, http.StatusOK, w.Code)
	var list []*Resp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list), "body: %s", w.Body.String())
	for _, r := range list {
		assert.NotEqual(t, hidden.ProjectID, r.ProjectID,
			"the P0 user list must not enumerate unlisted projects for a Space admin; "+
				"that is the P2 admin surface's job")
	}
}
