package project

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------- helpers ----------

func setPinned(t *testing.T, srv *server.Server, projectID, token string, pinned bool) *httptest.ResponseRecorder {
	t.Helper()
	return doJSON(t, srv, http.MethodPut, "/v1/projects/"+projectID+"/setting", token,
		map[string]any{"pinned": pinned})
}

func listProjectIDs(t *testing.T, srv *server.Server, spaceID, token string) []string {
	t.Helper()
	w := doJSON(t, srv, http.MethodGet, "/v1/space/"+spaceID+"/projects", token, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resps []*Resp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resps), "body: %s", w.Body.String())
	out := make([]string, 0, len(resps))
	for _, r := range resps {
		out = append(out, r.ProjectID)
	}
	return out
}

func pinnedFlags(t *testing.T, srv *server.Server, spaceID, token string) map[string]bool {
	t.Helper()
	w := doJSON(t, srv, http.MethodGet, "/v1/space/"+spaceID+"/projects", token, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resps []*Resp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resps), "body: %s", w.Body.String())
	out := map[string]bool{}
	for _, r := range resps {
		out[r.ProjectID] = r.Pinned
	}
	return out
}

// ---------- pinning ----------

// TestPinnedProjectSortsFirstAndLeavesTheRestAlone is the whole read-side contract.
//
// Two properties, and the second is the one a reviewer should look for: pinning
// must reorder the list WITHOUT disturbing the relative order of everything else.
// GET /v1/space/:space_id/projects already ships, so its existing order is a wire
// contract; a pin that shuffled the unpinned tail would be a behaviour change
// nobody asked for riding along with a feature.
func TestPinnedProjectSortsFirstAndLeavesTheRestAlone(t *testing.T) {
	srv, _ := setup(t)
	stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	tok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)

	first := createProjectVia(t, srv, spaceA, tok, "pin-a")
	second := createProjectVia(t, srv, spaceA, tok, "pin-b")
	third := createProjectVia(t, srv, spaceA, tok, "pin-c")

	// The list is newest-first (ORDER BY p.id DESC), so this is the baseline the
	// pin has to preserve for the rows it does not touch.
	require.Equal(t,
		[]string{third.ProjectID, second.ProjectID, first.ProjectID},
		listProjectIDs(t, srv, spaceA, tok))

	w := setPinned(t, srv, first.ProjectID, tok, true)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	assert.Equal(t,
		[]string{first.ProjectID, third.ProjectID, second.ProjectID},
		listProjectIDs(t, srv, spaceA, tok),
		"the pinned project leads, and the unpinned tail keeps the order it had")
}

// TestPinIsPerUser pins the field's whole reason for living in its own table: the
// same project row reads pinned for one caller and not for the next.
func TestPinIsPerUser(t *testing.T) {
	srv, _ := setup(t)
	stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	ownerTok := seedUser(t, "owner1")
	mateTok := seedUser(t, "mate")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	seedSpaceMember(t, spaceA, "mate", 0, 1)

	created := createProjectVia(t, srv, spaceA, ownerTok, "pin-per-user")
	require.Equal(t, http.StatusOK, setPinned(t, srv, created.ProjectID, ownerTok, true).Code)

	assert.True(t, pinnedFlags(t, srv, spaceA, ownerTok)[created.ProjectID],
		"the caller who pinned it sees pinned = true")
	assert.False(t, pinnedFlags(t, srv, spaceA, mateTok)[created.ProjectID],
		"another Space member sees the same project unpinned; a pin stored on the "+
			"project row rather than per user would leak one caller's preference to "+
			"everyone")
}

// TestPinDoesNotMoveMemberEpoch is the reason this is a separate table rather than
// a column on octo_project_member.
//
// member_epoch is what fleet and drive poll to decide whether a project's
// membership changed. Every write to octo_project_member sits on the +1 path, so a
// pin stored there would either bump the epoch — telling every subsystem to
// re-reconcile a roster that did not change — or need a carve-out on the one write
// path this module keeps free of them.
func TestPinDoesNotMoveMemberEpoch(t *testing.T) {
	srv, _ := setup(t)
	stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	tok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	created := createProjectVia(t, srv, spaceA, tok, "pin-epoch")

	before := epochOf(t, created.ProjectID)
	require.Equal(t, http.StatusOK, setPinned(t, srv, created.ProjectID, tok, true).Code)
	require.Equal(t, http.StatusOK, setPinned(t, srv, created.ProjectID, tok, false).Code)

	assert.Equal(t, before, epochOf(t, created.ProjectID),
		"pinning is not a membership change and must not make every subsystem "+
			"re-reconcile a roster that did not move")
}

// TestPinIsIdempotent covers what makes PUT the right verb: pinning twice rewrites
// one row rather than growing a second, which the unique key gives for free and the
// upsert relies on.
func TestPinIsIdempotent(t *testing.T) {
	srv, _ := setup(t)
	stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	tok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	created := createProjectVia(t, srv, spaceA, tok, "pin-idempotent")

	for i := 0; i < 3; i++ {
		require.Equal(t, http.StatusOK, setPinned(t, srv, created.ProjectID, tok, true).Code,
			"repeat %d must succeed, not conflict", i)
	}

	var n int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM octo_project_user_setting WHERE project_id = ? AND uid = ?",
		created.ProjectID, "owner1").LoadOne(&n))
	assert.Equal(t, 1, n, "the unique key must collapse repeats into one row")
	assert.True(t, pinnedFlags(t, srv, spaceA, tok)[created.ProjectID])
}

// TestUnpinRestoresTheOriginalOrder covers the other half of the toggle, and pins
// that pinned_at is CLEARED on unpin rather than left behind: a stale timestamp
// would order a later re-pin by when it was first pinned, so a project the caller
// just re-pinned would not come back to the front.
func TestUnpinRestoresTheOriginalOrder(t *testing.T) {
	srv, _ := setup(t)
	stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	tok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)

	first := createProjectVia(t, srv, spaceA, tok, "unpin-a")
	second := createProjectVia(t, srv, spaceA, tok, "unpin-b")

	require.Equal(t, http.StatusOK, setPinned(t, srv, first.ProjectID, tok, true).Code)
	require.Equal(t, []string{first.ProjectID, second.ProjectID},
		listProjectIDs(t, srv, spaceA, tok))

	require.Equal(t, http.StatusOK, setPinned(t, srv, first.ProjectID, tok, false).Code)
	assert.Equal(t, []string{second.ProjectID, first.ProjectID},
		listProjectIDs(t, srv, spaceA, tok),
		"unpinning must put the project back where it was, not leave it floating")

	// Asserted in SQL rather than scanned into Go: the column is nullable and NULL
	// is the whole point, so a scan type that cannot hold it would fail for the
	// wrong reason.
	var rows, cleared int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM octo_project_user_setting WHERE project_id = ? AND uid = ?",
		first.ProjectID, "owner1").LoadOne(&rows))
	require.Equal(t, 1, rows, "the unpin must rewrite the row, not delete it")
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM octo_project_user_setting "+
			"WHERE project_id = ? AND uid = ? AND pinned_at IS NULL",
		first.ProjectID, "owner1").LoadOne(&cleared))
	assert.Equal(t, 1, cleared, "pinned_at must be cleared on unpin, or a later re-pin "+
		"sorts by the FIRST time it was pinned and does not come back to the front")
}

// TestRepinMovesToTheFront is the property the cleared pinned_at buys, asserted
// through the API rather than through the column.
func TestRepinMovesToTheFront(t *testing.T) {
	srv, _ := setup(t)
	stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	tok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)

	older := createProjectVia(t, srv, spaceA, tok, "repin-a")
	newer := createProjectVia(t, srv, spaceA, tok, "repin-b")

	require.Equal(t, http.StatusOK, setPinned(t, srv, older.ProjectID, tok, true).Code)
	require.Equal(t, http.StatusOK, setPinned(t, srv, newer.ProjectID, tok, true).Code)
	require.Equal(t, []string{newer.ProjectID, older.ProjectID},
		listProjectIDs(t, srv, spaceA, tok), "most recently pinned leads")

	require.Equal(t, http.StatusOK, setPinned(t, srv, older.ProjectID, tok, false).Code)
	require.Equal(t, http.StatusOK, setPinned(t, srv, older.ProjectID, tok, true).Code)
	assert.Equal(t, []string{older.ProjectID, newer.ProjectID},
		listProjectIDs(t, srv, spaceA, tok),
		"re-pinning must move the project to the front; if pinned_at survived the "+
			"unpin it would still sort by the original pin time")
}

// TestPinnedIsReportedByEveryRouteThatReturnsAProject pins the contract
// all_member_group_no already had to be fixed for once: a field on the list route
// and absent from the detail route makes the two disagree about the same project.
func TestPinnedIsReportedByEveryRouteThatReturnsAProject(t *testing.T) {
	srv, _ := setup(t)
	stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	tok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	created := createProjectVia(t, srv, spaceA, tok, "pin-routes")

	assert.False(t, created.Pinned, "a freshly created project cannot be pinned yet")

	put := setPinned(t, srv, created.ProjectID, tok, true)
	require.Equal(t, http.StatusOK, put.Code, "body: %s", put.Body.String())
	assert.True(t, decodeResp(t, put).Pinned,
		"the settings response must report the state it just wrote, so the client "+
			"can render from it instead of refetching the list")

	detail := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID, tok, nil)
	require.Equal(t, http.StatusOK, detail.Code, "body: %s", detail.Body.String())
	assert.True(t, decodeResp(t, detail).Pinned, "the detail route must agree with the list")

	// A rename must not silently un-pin the card in the update response.
	upd := doJSON(t, srv, http.MethodPut, "/v1/projects/"+created.ProjectID, tok,
		map[string]any{"name": "pin-routes-renamed"})
	require.Equal(t, http.StatusOK, upd.Code, "body: %s", upd.Body.String())
	assert.True(t, decodeResp(t, upd).Pinned,
		"the pin survives a rename, so the update response must re-read it rather "+
			"than defaulting to false")

	assert.True(t, pinnedFlags(t, srv, spaceA, tok)[created.ProjectID])
}

// TestASpaceAdminCanPinAProjectTheyNeverJoined pins the decision not to gate this
// on project membership.
//
// A space_listed project is visible to any Space member, so it is in their list and
// they can reasonably want it at the top. Requiring a project seat would make the
// endpoint refuse a caller who can see the row it refuses to let them pin.
func TestASpaceAdminCanPinAProjectTheyNeverJoined(t *testing.T) {
	srv, _ := setup(t)
	stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	ownerTok := seedUser(t, "owner1")
	adminTok := seedUser(t, "spaceadmin")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	seedSpaceMember(t, spaceA, "spaceadmin", 1, 1)
	created := createProjectVia(t, srv, spaceA, ownerTok, "pin-nonmember")

	w := setPinned(t, srv, created.ProjectID, adminTok, true)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.True(t, pinnedFlags(t, srv, spaceA, adminTok)[created.ProjectID])
	assert.False(t, pinnedFlags(t, srv, spaceA, ownerTok)[created.ProjectID],
		"and it stays their own preference")
}

// TestSettingRefusalsAreIndistinguishable inherits projectMiddleware's
// anti-enumeration contract on the new write route, asserted against each other
// rather than against a status code.
func TestSettingRefusalsAreIndistinguishable(t *testing.T) {
	srv, _ := setup(t)
	stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	seedSpace(t, spaceB, 1)
	ownerTok := seedUser(t, "owner1")
	strangerTok := seedUser(t, "stranger")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	seedSpaceMember(t, spaceA, "stranger", 0, 1)
	foreignTok := seedUser(t, "owner2")
	seedSpaceMember(t, spaceB, "owner2", 0, 1)
	foreign := createProjectVia(t, srv, spaceB, foreignTok, "setting-foreign")

	unlisted := createProjectVia(t, srv, spaceA, ownerTok, "setting-unlisted")
	require.Equal(t, http.StatusOK, doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+unlisted.ProjectID, ownerTok,
		map[string]any{"discoverability": DiscoverabilityUnlisted}).Code)

	nonexistent := setPinned(t, srv, util.GenerUUID(), strangerTok, true)
	assertProjectErrorCode(t, nonexistent, "err.server.project.not_found")
	for name, w := range map[string]*httptest.ResponseRecorder{
		"cross-space": setPinned(t, srv, foreign.ProjectID, strangerTok, true),
		"unlisted":    setPinned(t, srv, unlisted.ProjectID, strangerTok, true),
	} {
		assert.Equal(t, nonexistent.Code, w.Code, "%s: status must match nonexistent", name)
		assert.JSONEq(t, nonexistent.Body.String(), w.Body.String(),
			"%s must be byte-identical to a nonexistent project, or the write route "+
				"becomes the enumeration oracle the read routes close", name)
	}
}

// TestEmptySettingBodyIsANoOp pins the pointer field: a client sending only the key
// it changed must not clear the ones it did not mention. With one preference that
// is invisible; it stops being invisible at the second.
func TestEmptySettingBodyIsANoOp(t *testing.T) {
	srv, _ := setup(t)
	stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	tok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	created := createProjectVia(t, srv, spaceA, tok, "setting-noop")

	require.Equal(t, http.StatusOK, setPinned(t, srv, created.ProjectID, tok, true).Code)

	w := doJSON(t, srv, http.MethodPut, "/v1/projects/"+created.ProjectID+"/setting", tok,
		map[string]any{})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.True(t, decodeResp(t, w).Pinned,
		"a body that mentions no preference must change none of them")
}

// TestSettingIsOnTheAuthenticatedGroup — same argument as the group list route:
// the chain is checked where it is declared (TestAuthChainOrder), and this covers
// the one thing that guard cannot see.
func TestSettingIsOnTheAuthenticatedGroup(t *testing.T) {
	src := readStripped(t, "api.go")
	if !strings.Contains(src, `projectScoped.PUT("/:project_id/setting"`) {
		t.Fatal("PUT /:project_id/setting must be registered on the projectScoped group, " +
			"which is what mounts AuthMiddleware, SharedUIDRateLimiter and projectMiddleware")
	}
}

// TestPinQuotaIsEnforcedPerSpace pins the cap and, more importantly, WHICH cap it
// is: six per Space, not six per user.
//
// The per-Space choice is what makes the limit explicable from where it is
// enforced. A global budget would let a pin in the Space the caller is looking at
// be refused because of pins in a Space they cannot see, and no error message can
// make that make sense.
func TestPinQuotaIsEnforcedPerSpace(t *testing.T) {
	srv, p := setup(t)
	stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	seedSpace(t, spaceB, 1)
	tok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	seedSpaceMember(t, spaceB, "owner1", 0, 1)

	max := p.cfg.MaxPinned
	require.Equal(t, 6, max, "the shipped default is 6; this case is written against it")

	created := make([]*Resp, 0, max+1)
	for i := 0; i <= max; i++ {
		created = append(created, createProjectVia(t, srv, spaceA, tok,
			fmt.Sprintf("quota-a-%d", i)))
	}
	for i := 0; i < max; i++ {
		require.Equal(t, http.StatusOK, setPinned(t, srv, created[i].ProjectID, tok, true).Code,
			"pin %d of %d must succeed", i+1, max)
	}

	over := setPinned(t, srv, created[max].ProjectID, tok, true)
	assertProjectErrorCode(t, over, "err.server.project.quota_pinned")
	env := decodeProjectEnvelope(t, over.Body.Bytes())
	assert.EqualValues(t, max, env.Error.Details["max"],
		"the limit travels in details so the client does not hardcode it and drift")
	assert.False(t, pinnedFlags(t, srv, spaceA, tok)[created[max].ProjectID],
		"a refused pin must not have landed")

	// The budget is per Space, so a full Space A leaves Space B untouched.
	other := createProjectVia(t, srv, spaceB, tok, "quota-b-0")
	require.Equal(t, http.StatusOK, setPinned(t, srv, other.ProjectID, tok, true).Code,
		"Space A being full must not spend Space B's budget")
}

// TestUnpinIsNeverRefusedAtTheCap covers the escape hatch: the operation that
// LOWERS the count must work at the cap, or a user who reaches it is stuck.
func TestUnpinIsNeverRefusedAtTheCap(t *testing.T) {
	srv, p := setup(t)
	stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	tok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)

	max := p.cfg.MaxPinned
	created := make([]*Resp, 0, max+1)
	for i := 0; i <= max; i++ {
		created = append(created, createProjectVia(t, srv, spaceA, tok,
			fmt.Sprintf("unpin-cap-%d", i)))
	}
	for i := 0; i < max; i++ {
		require.Equal(t, http.StatusOK, setPinned(t, srv, created[i].ProjectID, tok, true).Code)
	}
	// The wire status is a pinned 400 (D14 compat) whatever the code's real
	// HTTPStatus is, so the refusal is asserted by code rather than by status.
	assertProjectErrorCode(t, setPinned(t, srv, created[max].ProjectID, tok, true),
		"err.server.project.quota_pinned")

	require.Equal(t, http.StatusOK, setPinned(t, srv, created[0].ProjectID, tok, false).Code,
		"unpinning at the cap must work; refusing it strands the user at the one "+
			"operation that would free a slot")
	assert.Equal(t, http.StatusOK, setPinned(t, srv, created[max].ProjectID, tok, true).Code,
		"and the freed slot must be usable")
}

// TestRepinningAtTheCapIsNotRefused pins the off-by-one the quota check invites:
// the project is already pinned and already counted, so re-pinning changes nothing
// and must not fail. A naive count >= max check refuses it — the shape of bug where
// turning a toggle on twice fails the second time.
func TestRepinningAtTheCapIsNotRefused(t *testing.T) {
	srv, p := setup(t)
	stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	tok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)

	max := p.cfg.MaxPinned
	var last *Resp
	for i := 0; i < max; i++ {
		last = createProjectVia(t, srv, spaceA, tok, fmt.Sprintf("repin-cap-%d", i))
		require.Equal(t, http.StatusOK, setPinned(t, srv, last.ProjectID, tok, true).Code)
	}

	assert.Equal(t, http.StatusOK, setPinned(t, srv, last.ProjectID, tok, true).Code,
		"re-pinning an already-pinned project at the cap adds no row and must succeed")
}

// TestDisbandedProjectsDoNotSpendThePinBudget pins the status filter in the count.
//
// A disbanded project is gone from every read path, so its owner cannot see it to
// unpin it. If its stale row still counted, the caller would be permanently one
// slot poorer with nothing to point at.
func TestDisbandedProjectsDoNotSpendThePinBudget(t *testing.T) {
	srv, p := setup(t)
	stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	tok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)

	max := p.cfg.MaxPinned
	created := make([]*Resp, 0, max+1)
	for i := 0; i <= max; i++ {
		created = append(created, createProjectVia(t, srv, spaceA, tok,
			fmt.Sprintf("disband-pin-%d", i)))
	}
	for i := 0; i < max; i++ {
		require.Equal(t, http.StatusOK, setPinned(t, srv, created[i].ProjectID, tok, true).Code)
	}
	assertProjectErrorCode(t, setPinned(t, srv, created[max].ProjectID, tok, true),
		"err.server.project.quota_pinned")

	w := doJSON(t, srv, http.MethodDelete, "/v1/projects/"+created[0].ProjectID, tok, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	assert.Equal(t, http.StatusOK, setPinned(t, srv, created[max].ProjectID, tok, true).Code,
		"a pin on a disbanded project must not hold a slot the caller can no longer free")
}
