package project

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The value 0 is reserved: the integration contract that consumes member_epoch
// defines it as "the project does not exist or is not visible". These tests pin
// that no real project can hold it, which is the only thing that makes that
// meaning true.
//
// They run against a real row rather than a stub on purpose. The collision they
// guard lived in the column default and the insert path, so a fake store would
// have reported whatever it was handed and agreed with itself.

// TestFreshProjectEpochIsNeverTheAbsentSentinel is the direct assertion.
//
// The failure it prevents: a solo project nobody has added to reports the same
// epoch as a disbanded one. A consumer caches a positive authorization answer
// under that epoch, the project is later disbanded, the epoch query filters on
// status and answers 0 for the now-absent project — and 0 equals the cached 0,
// so the consumer's staleness check AGREES and the grant survives. Forever,
// because the epoch never moves again.
func TestFreshProjectEpochIsNeverTheAbsentSentinel(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "epochBase")
	seedSpaceMember(t, spaceA, "epochBase", 0, 1)

	created := createProjectVia(t, srv, spaceA, token, "epoch-base")
	require.NotEmpty(t, created.ProjectID)

	row, err := testDB.queryByProjectID(created.ProjectID)
	require.NoError(t, err)
	require.NotNil(t, row)

	assert.Equal(t, StatusNormal, row.Status, "a freshly created project is active")
	assert.NotZero(t, row.MemberEpoch,
		"member_epoch 0 is the integration contract's sentinel for a project that does not "+
			"exist; an active project holding it makes a consumer's staleness check agree with "+
			"a disbanded project's answer")
	assert.EqualValues(t, 1, row.MemberEpoch, "creation starts the epoch at 1")

	// The response carries the same value; a client and the peer must not see
	// different epochs for the same project.
	assert.EqualValues(t, row.MemberEpoch, created.MemberEpoch,
		"the response epoch must match the stored one")

	members, err := testDB.countActiveMembers(created.ProjectID)
	require.NoError(t, err)
	assert.Equal(t, 1, members, "the owner seat exists, so this is a real project with a real member")
}

// TestEpochStillIncrementsFromTheNewBase checks the change did not cost the
// property the epoch exists for: it must still move on every membership write.
//
// Starting at 1 rather than 0 is only safe if 1 is a floor and not a plateau —
// if creation had been made to set the epoch and a later write had stopped
// bumping it, this suite would still have seen a non-zero value and passed.
func TestEpochStillIncrementsFromTheNewBase(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "epochBump")
	seedSpaceMember(t, spaceA, "epochBump", 0, 1)
	seedUser(t, "epochBumpTarget")
	seedSpaceMember(t, spaceA, "epochBumpTarget", 0, 1)

	created := createProjectVia(t, srv, spaceA, token, "epoch-bump")
	before, err := testDB.queryByProjectID(created.ProjectID)
	require.NoError(t, err)

	w := doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add",
		token, map[string]any{"uids": []string{"epochBumpTarget"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	after, err := testDB.queryByProjectID(created.ProjectID)
	require.NoError(t, err)
	assert.Greater(t, after.MemberEpoch, before.MemberEpoch,
		"a membership write must still advance the epoch from its new base")
}

// TestCreateWritesTheEpochExplicitly is the source-side half.
//
// The value has to be written by the INSERT rather than left to the column
// default, because the default is the reserved sentinel. A future edit that
// drops member_epoch back out of the insert column list would silently restore
// the collision on newly created projects only — invisible to any test that does
// not look at a fresh row, which is why this pins the mechanism as well as the
// outcome.
func TestCreateBumpsTheEpochRatherThanSeedingIt(t *testing.T) {
	create := funcBody(t, readLinesWithoutComments(t, "service.go"), "func (p *Project) createProjectOnce(")
	assert.True(t, strings.Contains(create, "bumpMemberEpochTx"),
		"createProjectOnce must move the epoch off the column default, or every new project "+
			"reports the value the integration contract reserves for a project that does not exist")

	// Via the shared increment, NOT by seeding the column at insert. member_epoch
	// may only ever be written as member_epoch + 1 — TestIsOfficialHasNoWriter
	// forbids it in the insert column list and TestMemberEpochOnlyEverIncrements
	// forbids every other write shape. Fixing the sentinel collision must not
	// come at the cost of that discipline, so this pins which mechanism was used.
	for _, col := range projectInsertColumns {
		assert.NotEqual(t, "member_epoch", col,
			"member_epoch must not be seeded through the insert column list")
	}

	// The bump has to follow the project insert: bumpMemberEpochTx is guarded on
	// status = StatusNormal, so before the row exists it matches nothing and
	// silently does nothing — a failure with no error and no test, unless pinned.
	insertAt := strings.Index(create, "insertProjectTx")
	bumpAt := strings.Index(create, "bumpMemberEpochTx")
	require.Greater(t, insertAt, -1, "createProjectOnce must insert the project row")
	require.Greater(t, bumpAt, insertAt,
		"the epoch bump must come AFTER the project insert, or its status guard matches no row")
}
