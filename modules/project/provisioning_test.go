package project

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/internal/projectprovision"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------- fixtures ----------

const provTestSecretFleet = "fleet-secret-0123456789abcdefghij"
const provTestSecretDrive = "drive-secret-0123456789abcdefghij"

// fakeTarget stands in for a subsystem's ensure endpoint.
//
// It models the ONE property the brief makes a precondition (P-1): ensure is keyed
// on the supplied container_id and is idempotent, so it records containers in a map
// and counts calls separately. A test can therefore distinguish "called twice" from
// "created twice", which is exactly what the at-least-once acceptance item asks
// about and what a plain call counter cannot answer.
type fakeTarget struct {
	*httptest.Server
	mu         sync.Mutex
	calls      int
	containers map[string]int
	status     int
	// respondWith overrides the echoed container id when non-empty, for the
	// mismatch case.
	respondWith string
}

func newFakeTarget(t *testing.T) *fakeTarget {
	t.Helper()
	f := &fakeTarget{containers: map[string]int{}, status: http.StatusOK}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req projectprovision.EnsureRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.calls++
		status := f.status
		echo := req.ContainerID
		if f.respondWith != "" {
			echo = f.respondWith
		}
		if status == http.StatusOK {
			f.containers[req.ContainerID]++
		}
		f.mu.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(projectprovision.EnsureResponse{ContainerID: echo, Slug: echo})
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeTarget) snapshot() (calls int, containers map[string]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]int, len(f.containers))
	for k, v := range f.containers {
		out[k] = v
	}
	return f.calls, out
}

func (f *fakeTarget) setStatus(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
}

// enableProvisioning turns targets on AFTER the routes are mounted, and neutralizes
// the post-create nudge.
//
// The ordering is load-bearing, not cosmetic. startProvisioningWorker runs from
// Route() and is wrapped in a process-wide sync.Once; enabling a target before the
// mount would schedule real timers that then run batches concurrently with every
// later case in this package's single test binary. Enabling afterwards leaves the
// Once unconsumed (the function returns before Do when no target is enabled), so the
// only thing that ever calls out is the explicit processProvisioningJobs() in a test.
//
// The nudge is replaced rather than tolerated for the same reason: it is a goroutine,
// so any assertion about a row still being `pending` would be a race.
func enableProvisioning(t *testing.T, p *Project, targets ...provisionTarget) {
	t.Helper()
	p.nudgeProvisioningFn = func() {}
	p.cfg.Provisioning = ProvisioningConfig{
		Targets:     targets,
		Interval:    time.Hour,
		Timeout:     2 * time.Second,
		MaxAttempts: defaultProvisionMaxAttempts,
		BatchSize:   defaultProvisionBatch,
	}
}

func fleetTargetOn(f *fakeTarget) provisionTarget {
	return provisionTarget{Target: projectprovision.Target{
		Name: TargetFleet, EnsureURL: f.URL + "/api/internal/workspaces/ensure",
		Secret: provTestSecretFleet, Timeout: 2 * time.Second,
	}}
}

func driveTargetOn(f *fakeTarget) provisionTarget {
	return provisionTarget{Target: projectprovision.Target{
		Name: TargetDrive, EnsureURL: f.URL + "/v1/internal/drive/spaces/ensure",
		Secret: provTestSecretDrive, Timeout: 2 * time.Second,
	}}
}

// provRow mirrors one octo_project_provisioning row for assertions.
type provRow struct {
	ID          uint64     `db:"id"`
	ProjectID   string     `db:"project_id"`
	SpaceID     string     `db:"space_id"`
	Target      string     `db:"target"`
	ContainerID string     `db:"container_id"`
	Status      uint8      `db:"status"`
	Attempts    uint32     `db:"attempts"`
	LeaseOwner  string     `db:"lease_owner"`
	LastError   string     `db:"last_error"`
	FinishedAt  *time.Time `db:"finished_at"`
}

// readProvisioningRows returns every row for a project, ordered by target so the
// assertions are stable. Named read* because provisioningRows is the metric.
func readProvisioningRows(t *testing.T, projectID string) []provRow {
	t.Helper()
	var rows []provRow
	_, err := testCtx.DB().SelectBySql(
		"SELECT id, project_id, space_id, target, container_id, status, attempts, lease_owner, last_error, finished_at "+
			"FROM `octo_project_provisioning` WHERE project_id = ? ORDER BY target",
		projectID,
	).Load(&rows)
	require.NoError(t, err)
	return rows
}

func countAllProvisioningRows(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM `octo_project_provisioning`").LoadOne(&n))
	return n
}

// provisioningSetup builds a clean Project with a private router and the given
// targets enabled.
func provisioningSetup(t *testing.T, targets ...*fakeTarget) (*Project, *wkhttp.WKHttp, string) {
	t.Helper()
	_, p := setup(t)
	r := mountProject(t, p)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	var resolved []provisionTarget
	for i, f := range targets {
		if i == 0 {
			resolved = append(resolved, fleetTargetOn(f))
		} else {
			resolved = append(resolved, driveTargetOn(f))
		}
	}
	enableProvisioning(t, p, resolved...)
	return p, r, token
}

func createVia(t *testing.T, r *wkhttp.WKHttp, token, name string) *Resp {
	t.Helper()
	w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", token,
		map[string]any{"name": name})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	return decodeResp(t, w)
}

// ---------- enqueue ----------

// TestCreateEnqueuesExactlyOneRowPerEnabledTarget is the committed-create half of the
// acceptance item; the rollback half is the next test.
func TestCreateEnqueuesExactlyOneRowPerEnabledTarget(t *testing.T) {
	fleet, drive := newFakeTarget(t), newFakeTarget(t)
	_, r, token := provisioningSetup(t, fleet, drive)

	created := createVia(t, r, token, "P")
	rows := readProvisioningRows(t, created.ProjectID)
	require.Len(t, rows, 2)

	seen := map[string]bool{}
	for _, row := range rows {
		assert.Equal(t, provisionStatusPending, row.Status)
		assert.Equal(t, uint32(0), row.Attempts)
		assert.Equal(t, spaceA, row.SpaceID)
		assert.Nil(t, row.FinishedAt)
		assert.Empty(t, row.LastError)
		// The container id is present from t=0. Nothing is backfilled from the
		// target's response, which is what makes at-least-once delivery idempotent
		// on a key the receiver enforces (D1).
		assert.NotEmpty(t, row.ContainerID, "container_id must be set at enqueue, not backfilled")
		require.False(t, seen[row.ContainerID], "two targets got the same container id")
		seen[row.ContainerID] = true
	}
	assert.Equal(t, TargetDrive, rows[0].Target)
	assert.Equal(t, TargetFleet, rows[1].Target)
	assert.True(t, strings.HasPrefix(rows[0].ContainerID, "octods-"))
	assert.True(t, strings.HasPrefix(rows[1].ContainerID, "octows-"))

	// Nothing has been called yet: enqueue is a database write, not an egress.
	calls, _ := fleet.snapshot()
	assert.Zero(t, calls, "enqueue must not call the target on the request path")
}

// TestProvisioningEnqueueFailureRollsBackTheWholeCreate is the rollback half, and it
// is the test that actually proves the enqueue is INSIDE the create transaction.
//
// A create that fails before the enqueue would leave no rows either, so it could not
// distinguish "in the transaction" from "after the commit". Failing the enqueue
// itself can: if it ran after the commit, the octo_project row would survive.
//
// The failure is injected through an unknown target name, which is the one input
// newContainerID rejects.
func TestProvisioningEnqueueFailureRollsBackTheWholeCreate(t *testing.T) {
	_, p := setup(t)
	r := mountProject(t, p)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	enableProvisioning(t, p, provisionTarget{Target: projectprovision.Target{
		Name: "not-a-real-target", EnsureURL: "https://x.internal/ensure", Secret: provTestSecretFleet,
	}})

	rejectedBefore := testutil.ToFloat64(writeRejected.WithLabelValues(entryProjectCreate, reasonProvisioningEnqueue))
	w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", token,
		map[string]any{"name": "P"})
	require.NotEqual(t, http.StatusOK, w.Code, "create must fail closed when the outbox write fails")
	// Its own metric reason, so "creation is failing" and "creation is failing because
	// the provisioning slice was turned on" are distinguishable — this slice is what
	// made create depend on the outbox write.
	assert.Equal(t, rejectedBefore+1,
		testutil.ToFloat64(writeRejected.WithLabelValues(entryProjectCreate, reasonProvisioningEnqueue)))

	assert.Zero(t, countAllProvisioningRows(t))
	var projects int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM `octo_project`").LoadOne(&projects))
	assert.Zero(t, projects, "the project row survived a failed enqueue: the enqueue is not in the create transaction")
	var members int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM `octo_project_member`").LoadOne(&members))
	assert.Zero(t, members)
}

// TestProvisioningIsInertWithNoTargetEnabled pins the default: create behaves exactly
// as it does without this slice.
func TestProvisioningIsInertWithNoTargetEnabled(t *testing.T) {
	_, p := setup(t)
	r := mountProject(t, p)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	require.False(t, p.cfg.Provisioning.Enabled())

	created := createVia(t, r, token, "P")
	assert.Empty(t, readProvisioningRows(t, created.ProjectID))
	// And the worker refuses to run at all, so a stray tick cannot claim anything.
	p.processProvisioningJobs()
	assert.Zero(t, countAllProvisioningRows(t))
}

// TestEnqueueDuplicateKeyErrorCarriesNoDatabaseText covers the one enqueue failure whose
// MySQL message embeds a value verbatim.
//
// "Duplicate entry '<value>' for key '<index>'" is how 1062 reads, and on
// uk_octo_project_provisioning_container that value IS a container id — which then
// reaches a log line through errors.Join and zap.Error. Reaching that specific key needs
// a 128-bit collision, so the collision this test can actually force is the sibling one
// on (project_id, target); both go through the same branch, and what the branch has to
// guarantee is that no database text survives it.
//
// The zap-field guard cannot cover this: it inspects zap arguments, and a leak arriving
// inside an error string is invisible to it.
func TestEnqueueDuplicateKeyErrorCarriesNoDatabaseText(t *testing.T) {
	fleet := newFakeTarget(t)
	p, r, token := provisioningSetup(t, fleet)
	created := createVia(t, r, token, "P")

	tx, err := testCtx.DB().Begin()
	require.NoError(t, err)
	defer tx.RollbackUnlessCommitted()
	// Same (project_id, target) as the row the create already wrote.
	err = p.db.enqueueProvisioningTx(tx, created.ProjectID, spaceA,
		[]provisionTarget{fleetTargetOn(fleet)}, time.Now().UTC())

	require.Error(t, err)
	require.ErrorIs(t, err, errProvisioningEnqueueFailed,
		"the caller classifies on this sentinel to pick its metric reason")
	for _, banned := range []string{"Duplicate entry", "1062", "uk_octo_project_provisioning", "octows-"} {
		assert.NotContains(t, err.Error(), banned,
			"the duplicate-key error still carries database text; on the container-id key that "+
				"text is a capability and this chain is logged with zap.Error")
	}
}

// ---------- container id opacity ----------

// TestContainerIDIsNotAFunctionOfProjectID is the behavioural half of D1's
// non-derivability requirement; the source half is in provisioning_guard_test.go.
func TestContainerIDIsNotAFunctionOfProjectID(t *testing.T) {
	fleet := newFakeTarget(t)
	_, r, token := provisioningSetup(t, fleet)

	first := createVia(t, r, token, "P1")
	second := createVia(t, r, token, "P2")

	a := readProvisioningRows(t, first.ProjectID)
	b := readProvisioningRows(t, second.ProjectID)
	require.Len(t, a, 1)
	require.Len(t, b, 1)

	for _, row := range []provRow{a[0], b[0]} {
		assert.NotContains(t, row.ContainerID, row.ProjectID,
			"the container id embeds the project id, so anyone who can list projects can derive it")
		// 16 random bytes as hex, behind the 7-character target prefix.
		assert.Len(t, row.ContainerID, len("octows-")+2*containerIDRandomBytes)
	}
	assert.NotEqual(t, a[0].ContainerID, b[0].ContainerID)

	// Two ids minted for the SAME target must not share anything beyond the prefix,
	// which is what rules out a counter or a timestamp behind the prefix.
	assert.NotEqual(t, a[0].ContainerID[len("octows-"):], b[0].ContainerID[len("octows-"):])
}

// TestNoClientResponseCarriesAContainerID is the disclosure guard.
//
// PR-5 provisions but discloses nothing: until the target declares Project
// narrowing, the opacity of the id is the only thing between an eagerly created
// container and every member of its Space (brief R1).
func TestNoClientResponseCarriesAContainerID(t *testing.T) {
	fleet, drive := newFakeTarget(t), newFakeTarget(t)
	p, r, token := provisioningSetup(t, fleet, drive)

	created := createVia(t, r, token, "P")
	rows := readProvisioningRows(t, created.ProjectID)
	require.Len(t, rows, 2)
	// Drive the worker to ready first: a container id that leaks only AFTER
	// provisioning succeeds would slip past a create-only assertion.
	p.processProvisioningJobs()

	bodies := map[string]string{}
	w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", token, map[string]any{"name": "P2"})
	bodies["create"] = w.Body.String()
	w = doOn(t, r, http.MethodGet, "/v1/projects/"+created.ProjectID, token, nil)
	require.Equal(t, http.StatusOK, w.Code)
	bodies["detail"] = w.Body.String()
	w = doOn(t, r, http.MethodGet, "/v1/space/"+spaceA+"/projects", token, nil)
	require.Equal(t, http.StatusOK, w.Code)
	bodies["list"] = w.Body.String()
	w = doOn(t, r, http.MethodGet, "/v1/projects/"+created.ProjectID+"/members", token, nil)
	require.Equal(t, http.StatusOK, w.Code)
	bodies["members"] = w.Body.String()

	// verify is named explicitly by the acceptance item, and it is the response this
	// module does not own — which is why it is asserted rather than reasoned about. It is
	// served by the SHARED test router (module.Setup mounted every module into this test
	// binary), not the private one; the row it must not disclose lives in the same
	// database either way.
	//
	// It is also where a container id would most plausibly appear by accident: verify is
	// the endpoint every subsystem fronts, so "tell me which containers this user's
	// projects have" is a natural-sounding thing for someone to add to it.
	//
	// appconfig is the fourth response the acceptance item names and it is NOT asserted
	// here, deliberately: CleanAllTables removes the app_config row and recreating a valid
	// one means generating an RSA keypair and encrypting it with the master key, i.e.
	// reimplementing modules/common.insertAppConfigIfNeed inside this test. Asserting
	// against its 400 body instead would be a test that passes because there is nothing in
	// the response at all. It is covered STRUCTURALLY instead, and more strongly, by
	// TestNoPackageOutsideTheProvisioningSliceCanReadAContainerID — which covers every
	// response every other module produces rather than the one shape a test happened to
	// hit.
	w = doJSON(t, testSrv, http.MethodPost, "/v1/auth/verify", token, map[string]any{"token": token})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	bodies["verify"] = w.Body.String()

	for name, body := range bodies {
		for _, row := range readProvisioningRows(t, created.ProjectID) {
			assert.NotContains(t, body, row.ContainerID, "%s response discloses a container id", name)
		}
		// Also catch a field that would carry it later under a different value.
		assert.NotContains(t, strings.ToLower(body), "container", "%s response mentions a container field", name)
		assert.NotContains(t, strings.ToLower(body), "provision", "%s response mentions provisioning state", name)
	}
}

// ---------- worker ----------

// TestWorkerReachesReadyAndConvergesOnReplay covers the idempotency acceptance item.
//
// The replay is forced by putting the row back to pending, which is what
// at-least-once delivery looks like from the target's side (a lease expiry, a lost
// terminal write). The fake target keys containers by container_id, so "called
// twice, created once" is assertable.
func TestWorkerReachesReadyAndConvergesOnReplay(t *testing.T) {
	fleet := newFakeTarget(t)
	p, r, token := provisioningSetup(t, fleet)
	created := createVia(t, r, token, "P")

	p.processProvisioningJobs()
	rows := readProvisioningRows(t, created.ProjectID)
	require.Len(t, rows, 1)
	require.Equal(t, provisionStatusReady, rows[0].Status, "last_error=%q", rows[0].LastError)
	require.NotNil(t, rows[0].FinishedAt)
	assert.Equal(t, uint32(1), rows[0].Attempts)

	requeueProvisioningRow(t, rows[0].ID)
	p.processProvisioningJobs()

	after := readProvisioningRows(t, created.ProjectID)
	require.Len(t, after, 1, "a replay created a second outbox row")
	assert.Equal(t, provisionStatusReady, after[0].Status)
	calls, containers := fleet.snapshot()
	assert.Equal(t, 2, calls, "the replay should have called the target again")
	require.Len(t, containers, 1, "the replay created a second container")
	assert.Equal(t, 2, containers[rows[0].ContainerID])
}

// requeueProvisioningRow puts a terminal row back to pending and makes it due, so a
// test can drive a redelivery without waiting out the backoff.
func requeueProvisioningRow(t *testing.T, id uint64) {
	t.Helper()
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE `octo_project_provisioning` SET status = ?, attempts = 0, finished_at = NULL, "+
			"next_attempt_at = ?, lease_owner = '', lease_until = NULL WHERE id = ?",
		provisionStatusPending, time.Now().UTC().Add(-time.Minute), id,
	).Exec()
	require.NoError(t, err)
}

// makeProvisioningRowDue clears the backoff so the next batch picks the row up.
func makeProvisioningRowDue(t *testing.T, id uint64) {
	t.Helper()
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE `octo_project_provisioning` SET next_attempt_at = ? WHERE id = ?",
		time.Now().UTC().Add(-time.Minute), id,
	).Exec()
	require.NoError(t, err)
}

// TestWorkerRetriesThenAbandons pins the terminal failure path, including that
// last_error is bounded and carries no container id.
func TestWorkerRetriesThenAbandons(t *testing.T) {
	fleet := newFakeTarget(t)
	p, r, token := provisioningSetup(t, fleet)
	p.cfg.Provisioning.MaxAttempts = 2
	fleet.setStatus(http.StatusInternalServerError)

	created := createVia(t, r, token, "P")
	rows := readProvisioningRows(t, created.ProjectID)
	require.Len(t, rows, 1)
	id, containerID := rows[0].ID, rows[0].ContainerID

	p.processProvisioningJobs()
	rows = readProvisioningRows(t, created.ProjectID)
	require.Equal(t, provisionStatusPending, rows[0].Status, "the first failure must schedule a retry, not give up")
	assert.Equal(t, uint32(1), rows[0].Attempts)
	assert.Contains(t, rows[0].LastError, "target_5xx")
	assert.NotContains(t, rows[0].LastError, containerID, "last_error leaks the container id")

	attemptsBefore := testutil.ToFloat64(provisioningAttempts.WithLabelValues(TargetFleet, "target_5xx"))
	makeProvisioningRowDue(t, id)
	p.processProvisioningJobs()
	rows = readProvisioningRows(t, created.ProjectID)
	assert.Equal(t, provisionStatusAbandoned, rows[0].Status)
	// The attempt that EXHAUSTS the budget is counted once, under its real reason.
	// Previously it was counted twice — once as target_5xx and again as "abandoned" —
	// which made sum by(outcome) exceed the attempt count and hid the reason a row gave
	// up behind a label that only said that it did.
	assert.Equal(t, attemptsBefore+1,
		testutil.ToFloat64(provisioningAttempts.WithLabelValues(TargetFleet, "target_5xx")),
		"the exhausting attempt must be counted exactly once, with its real outcome")
	// And the second increment is gone. Asserting the real outcome alone cannot see the
	// double count — target_5xx went up by one either way — so the property has to be
	// stated as "the abandoned label does not exist", which is what makes
	// sum by(outcome) equal the attempt count.
	assert.Equal(t, 0.0, testutil.ToFloat64(provisioningAttempts.WithLabelValues(TargetFleet, "abandoned")),
		`provisioning_attempts_total must have no "abandoned" outcome: it double-counted the `+
			`exhausting attempt and competed with the real reason. Use `+
			`provisioning_rows{status="abandoned"} for "how many rows gave up".`)
	assert.Equal(t, uint32(2), rows[0].Attempts)
	require.NotNil(t, rows[0].FinishedAt)
	assert.Contains(t, rows[0].LastError, "retries exhausted")
	assert.NotContains(t, rows[0].LastError, containerID)

	// An abandoned row is not claimable again, so a later tick cannot resurrect it
	// (which is what makes the abandoned gauge a standing figure rather than a blip).
	callsBefore, _ := fleet.snapshot()
	makeProvisioningRowDue(t, id)
	p.processProvisioningJobs()
	callsAfter, _ := fleet.snapshot()
	assert.Equal(t, callsBefore, callsAfter)
}

// panickingEnsurer is a provisionEnsurer that always panics.
//
// It exists because the panic path had no test at all, which is exactly why nobody
// noticed it recorded no metric: the recovery, the release and the backoff were all
// asserted only by the comment above them. provisionEnsurer is the seam that makes
// this injectable — that is the second thing the interface buys beyond keeping the
// outbound package out of api.go.
type panickingEnsurer struct{ calls int }

func (e *panickingEnsurer) Ensure(context.Context, projectprovision.Target, projectprovision.EnsureRequest) (projectprovision.EnsureResponse, error) {
	e.calls++
	panic("provisioning boom")
}

// TestWorkerRecoversFromAPanicAndCountsIt covers a path that previously had neither a
// test nor a metric.
//
// A panic caught only at the batch level would skip the release, leaving the row
// claimed with no last_error and no backoff — it would sit until the lease expired, be
// re-claimed, and panic again, burning a full lease period AND a batch slot on every
// cycle. attempts advances at claim time so it would still converge to abandoned, but
// slowly and invisibly.
func TestWorkerRecoversFromAPanicAndCountsIt(t *testing.T) {
	fleet := newFakeTarget(t)
	p, r, token := provisioningSetup(t, fleet)
	panicker := &panickingEnsurer{}
	p.provisionClient = panicker

	before := testutil.ToFloat64(provisioningAttempts.WithLabelValues(TargetFleet, "panic"))
	created := createVia(t, r, token, "P")

	// Must not propagate: one poisoned row cannot be allowed to kill the batch.
	require.NotPanics(t, p.processProvisioningJobs)
	require.Equal(t, 1, panicker.calls)

	rows := readProvisioningRows(t, created.ProjectID)
	require.Len(t, rows, 1)
	assert.Equal(t, provisionStatusPending, rows[0].Status, "a panic must schedule a retry, not strand the row")
	assert.Equal(t, uint32(1), rows[0].Attempts, "attempts must advance so a poisoned row converges to abandoned")
	assert.Empty(t, rows[0].LeaseOwner, "the lease must be released, not held until it expires")
	assert.Contains(t, rows[0].LastError, "panic")
	// The metric half. Without it a job that panics on every attempt is invisible in
	// provisioning_attempts_total until it abandons.
	assert.Equal(t, before+1, testutil.ToFloat64(provisioningAttempts.WithLabelValues(TargetFleet, "panic")))
}

// TestWorkerRetriesAJobWhoseTargetWentAway covers the other path that had no test and
// no metric: configuration changed between the claim and the run.
//
// Retry rather than abandon, because the operator may be mid-rollout — and if the
// target stays off, the claim-side filter simply stops picking the row up, so nothing
// spins.
func TestWorkerRetriesAJobWhoseTargetWentAway(t *testing.T) {
	fleet := newFakeTarget(t)
	p, r, token := provisioningSetup(t, fleet)
	created := createVia(t, r, token, "P")
	rows := readProvisioningRows(t, created.ProjectID)
	require.Len(t, rows, 1)

	// Claim with the target enabled, then disable it before running — the window the
	// claim-side filter cannot cover.
	owner := newProvisioningClaimOwner()
	job, err := p.db.claimProvisioningJob(owner, []string{TargetFleet}, p.cfg.Provisioning.MaxAttempts, time.Now().UTC())
	require.NoError(t, err)
	require.NotNil(t, job)
	p.cfg.Provisioning.Targets = nil

	before := testutil.ToFloat64(provisioningAttempts.WithLabelValues(TargetFleet, "target_disabled"))
	p.runProvisioningJob(job, owner)

	rows = readProvisioningRows(t, created.ProjectID)
	assert.Equal(t, provisionStatusPending, rows[0].Status)
	assert.Empty(t, rows[0].LeaseOwner)
	assert.Contains(t, rows[0].LastError, "target_disabled")
	calls, _ := fleet.snapshot()
	assert.Zero(t, calls, "a disabled target must not be contacted")
	assert.Equal(t, before+1,
		testutil.ToFloat64(provisioningAttempts.WithLabelValues(TargetFleet, "target_disabled")))
}

// TestWorkerLeavesRowsAloneForADisabledTarget pins the claim-side target filter:
// turning a target off must not burn its pending rows' retry budget against a
// destination nobody is listening on.
func TestWorkerLeavesRowsAloneForADisabledTarget(t *testing.T) {
	fleet, drive := newFakeTarget(t), newFakeTarget(t)
	p, r, token := provisioningSetup(t, fleet, drive)
	created := createVia(t, r, token, "P")

	// Drop drive from the enabled set, as a rollback would.
	p.cfg.Provisioning.Targets = []provisionTarget{fleetTargetOn(fleet)}
	p.processProvisioningJobs()

	rows := readProvisioningRows(t, created.ProjectID)
	require.Len(t, rows, 2)
	byTarget := map[string]provRow{rows[0].Target: rows[0], rows[1].Target: rows[1]}
	assert.Equal(t, provisionStatusReady, byTarget[TargetFleet].Status)
	assert.Equal(t, provisionStatusPending, byTarget[TargetDrive].Status)
	assert.Equal(t, uint32(0), byTarget[TargetDrive].Attempts,
		"a disabled target's rows must not consume attempts")
	driveCalls, _ := drive.snapshot()
	assert.Zero(t, driveCalls)
}

// TestSweepAbandonsAHardKilledJob covers the out-of-process complement to the release
// path: a pod killed mid-job leaves the row pending with its budget spent, and the
// claim-side bound means nothing would ever pick it up again.
func TestSweepAbandonsAHardKilledJob(t *testing.T) {
	fleet := newFakeTarget(t)
	p, r, token := provisioningSetup(t, fleet)
	p.cfg.Provisioning.MaxAttempts = 1
	created := createVia(t, r, token, "P")
	rows := readProvisioningRows(t, created.ProjectID)
	require.Len(t, rows, 1)

	// Exactly the state a SIGKILL leaves: attempts spent, lease held by a process
	// that no longer exists.
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE `octo_project_provisioning` SET attempts = ?, lease_owner = 'dead', lease_until = ? WHERE id = ?",
		p.cfg.Provisioning.MaxAttempts, time.Now().UTC().Add(-2*provisioningLease), rows[0].ID,
	).Exec()
	require.NoError(t, err)

	p.sweepExhaustedProvisioningJobs()
	rows = readProvisioningRows(t, created.ProjectID)
	assert.Equal(t, provisionStatusAbandoned, rows[0].Status)
	require.NotNil(t, rows[0].FinishedAt)
}

// TestSweepSparesAJobStillInsideItsLease is the other half, and it is why the grace
// period is a FULL lease rather than "the lease has expired": a job on its final
// attempt has attempts == max while its executor is still legitimately running, and
// writing a terminal state under it would make its own finish land on nothing.
func TestSweepSparesAJobStillInsideItsLease(t *testing.T) {
	fleet := newFakeTarget(t)
	p, r, token := provisioningSetup(t, fleet)
	p.cfg.Provisioning.MaxAttempts = 1
	created := createVia(t, r, token, "P")
	rows := readProvisioningRows(t, created.ProjectID)

	_, err := testCtx.DB().UpdateBySql(
		"UPDATE `octo_project_provisioning` SET attempts = ?, lease_owner = 'alive', lease_until = ? WHERE id = ?",
		p.cfg.Provisioning.MaxAttempts, time.Now().UTC().Add(time.Minute), rows[0].ID,
	).Exec()
	require.NoError(t, err)

	p.sweepExhaustedProvisioningJobs()
	rows = readProvisioningRows(t, created.ProjectID)
	assert.Equal(t, provisionStatusPending, rows[0].Status,
		"a job still holding its lease was swept to abandoned")
}

// ---------- disband ----------

// TestDisbandMovesRowsToDisbandPendingAndSendsNothing pins D9's pull-based teardown.
func TestDisbandMovesRowsToDisbandPendingAndSendsNothing(t *testing.T) {
	fleet, drive := newFakeTarget(t), newFakeTarget(t)
	p, r, token := provisioningSetup(t, fleet, drive)
	created := createVia(t, r, token, "P")
	p.processProvisioningJobs()

	fleetBefore, _ := fleet.snapshot()
	driveBefore, _ := drive.snapshot()

	w := doOn(t, r, http.MethodDelete, "/v1/projects/"+created.ProjectID, token, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	rows := readProvisioningRows(t, created.ProjectID)
	require.Len(t, rows, 2)
	for _, row := range rows {
		assert.Equal(t, provisionStatusDisbandPending, row.Status)
		require.NotNil(t, row.FinishedAt)
	}
	// Nothing outbound, on the disband itself or on a later tick: disband_pending is
	// not claimable, and the subsystem is expected to poll instead.
	p.processProvisioningJobs()
	fleetAfter, _ := fleet.snapshot()
	driveAfter, _ := drive.snapshot()
	assert.Equal(t, fleetBefore, fleetAfter, "disband sent something outbound to fleet")
	assert.Equal(t, driveBefore, driveAfter, "disband sent something outbound to drive")
}

// TestDisbandMovesAnAbandonedRowToo pins the choice recorded on
// markProvisioningDisbandPendingTx: an abandoned row for a project that no longer
// exists must not pin the abandoned gauge above zero forever.
func TestDisbandMovesAnAbandonedRowToo(t *testing.T) {
	fleet := newFakeTarget(t)
	p, r, token := provisioningSetup(t, fleet)
	p.cfg.Provisioning.MaxAttempts = 1
	fleet.setStatus(http.StatusInternalServerError)
	created := createVia(t, r, token, "P")
	p.processProvisioningJobs()
	rows := readProvisioningRows(t, created.ProjectID)
	require.Equal(t, provisionStatusAbandoned, rows[0].Status)

	w := doOn(t, r, http.MethodDelete, "/v1/projects/"+created.ProjectID, token, nil)
	require.Equal(t, http.StatusOK, w.Code)
	rows = readProvisioningRows(t, created.ProjectID)
	assert.Equal(t, provisionStatusDisbandPending, rows[0].Status)
	// The failure history survives the transition, so nothing is lost by clearing the
	// alert.
	assert.Contains(t, rows[0].LastError, "retries exhausted")
}

// ---------- purge ----------

// TestPurgeOnlyDeletesDisbandPendingRows pins the retention rule: ready rows are the
// local enumeration D12 keeps the table for, and abandoned rows are the only durable
// record that provisioning gave up.
func TestPurgeOnlyDeletesDisbandPendingRows(t *testing.T) {
	setup(t)
	old := time.Now().UTC().Add(-2 * provisioningRetention)
	for i, status := range []uint8{provisionStatusReady, provisionStatusAbandoned, provisionStatusDisbandPending} {
		_, err := testCtx.DB().InsertBySql(
			"INSERT INTO `octo_project_provisioning` "+
				"(project_id, space_id, target, container_id, status, next_attempt_at, created_at, finished_at) "+
				"VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			// Distinct project ids: (project_id, target) is unique, which is what makes
			// "exactly one row per target" structural.
			fmt.Sprintf("p%d", i), spaceA, TargetFleet, "octows-"+strings.Repeat("a", 30)+string(rune('0'+i)),
			status, old, old, old,
		).Exec()
		require.NoError(t, err)
	}
	deleted, err := testDB.purgeFinishedProvisioningJobs(time.Now().UTC().Add(-provisioningRetention), 100)
	require.NoError(t, err)
	assert.Equal(t, int64(1), deleted)
	assert.Equal(t, 2, countAllProvisioningRows(t))
}

// ---------- metrics ----------

// TestUnnarrowedContainerGaugeCountsReadyRowsOnUnnarrowedTargets covers the one
// observability item the brief makes a shipping requirement: the size of the surface
// that is protected by the opacity of the container id alone.
func TestUnnarrowedContainerGaugeCountsReadyRowsOnUnnarrowedTargets(t *testing.T) {
	fleet, drive := newFakeTarget(t), newFakeTarget(t)
	p, r, token := provisioningSetup(t, fleet, drive)
	// fleet has shipped R2, drive has not.
	p.cfg.Provisioning.Targets = []provisionTarget{
		{Target: fleetTargetOn(fleet).Target, Narrowed: true},
		{Target: driveTargetOn(drive).Target, Narrowed: false},
	}
	createVia(t, r, token, "P")
	p.processProvisioningJobs()
	p.refreshProvisioningMetrics()

	assert.Equal(t, 0.0, testutil.ToFloat64(provisioningUnnarrowedContainers.WithLabelValues(TargetFleet)),
		"a narrowed target must not be counted as exposed surface")
	assert.Equal(t, 1.0, testutil.ToFloat64(provisioningUnnarrowedContainers.WithLabelValues(TargetDrive)))
	assert.Equal(t, 1.0, testutil.ToFloat64(provisioningRows.WithLabelValues(TargetDrive, "ready")))
	assert.Equal(t, 0.0, testutil.ToFloat64(provisioningRows.WithLabelValues(TargetDrive, "pending")))

	// The gauge must fall back to 0 when the condition clears, rather than holding a
	// stale reading that reads as an unresolved alert.
	_, err := testCtx.DB().UpdateBySql("DELETE FROM `octo_project_provisioning`").Exec()
	require.NoError(t, err)
	p.refreshProvisioningMetrics()
	assert.Equal(t, 0.0, testutil.ToFloat64(provisioningUnnarrowedContainers.WithLabelValues(TargetDrive)))
	assert.Equal(t, 0.0, testutil.ToFloat64(provisioningRows.WithLabelValues(TargetDrive, "ready")))
}

// ---------- config ----------

// TestLoadProvisioningConfig covers the drop-a-bad-target contract and the credential
// exclusivity rules.
func TestLoadProvisioningConfig(t *testing.T) {
	const okSecretA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const okSecretB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}

	t.Run("default is inert", func(t *testing.T) {
		cfg, problems := loadProvisioningConfig(env(nil))
		assert.False(t, cfg.Enabled())
		assert.Empty(t, problems)
		assert.Equal(t, defaultProvisionInterval, cfg.Interval)
		assert.Equal(t, defaultProvisionMaxAttempts, cfg.MaxAttempts)
	})

	t.Run("both targets", func(t *testing.T) {
		cfg, problems := loadProvisioningConfig(env(map[string]string{
			envProvisionTargets:       " Fleet , drive ,",
			envProvisionFleetURL:      "https://fleet.internal/api/internal/workspaces/ensure",
			ProvisionFleetSecretEnv:   okSecretA,
			envProvisionDriveURL:      "https://drive.internal/v1/internal/drive/spaces/ensure",
			ProvisionDriveSecretEnv:   okSecretB,
			envProvisionFleetNarrowed: "true",
		}))
		require.Empty(t, problems)
		require.Len(t, cfg.Targets, 2)
		fleet, ok := cfg.TargetByName(TargetFleet)
		require.True(t, ok)
		assert.True(t, fleet.Narrowed, "the narrowing switch did not resolve")
		drive, ok := cfg.TargetByName(TargetDrive)
		require.True(t, ok)
		assert.False(t, drive.Narrowed)
	})

	t.Run("a bad target is dropped, not fatal", func(t *testing.T) {
		cfg, problems := loadProvisioningConfig(env(map[string]string{
			envProvisionTargets:     "fleet,drive",
			envProvisionFleetURL:    "https://fleet.internal/ensure",
			ProvisionFleetSecretEnv: okSecretA,
			// drive: no URL at all
			ProvisionDriveSecretEnv: okSecretB,
		}))
		require.Len(t, problems, 1)
		require.Len(t, cfg.Targets, 1)
		assert.Equal(t, TargetFleet, cfg.Targets[0].Name)
		assert.Equal(t, []string{TargetDrive}, cfg.Misconfigured)
		assert.True(t, cfg.Enabled(), "one bad target must not disable the good one")
	})

	t.Run("secret reuse across targets is refused", func(t *testing.T) {
		cfg, problems := loadProvisioningConfig(env(map[string]string{
			envProvisionTargets:     "fleet,drive",
			envProvisionFleetURL:    "https://fleet.internal/ensure",
			ProvisionFleetSecretEnv: okSecretA,
			envProvisionDriveURL:    "https://drive.internal/ensure",
			ProvisionDriveSecretEnv: okSecretA,
		}))
		require.Len(t, problems, 1)
		assert.Len(t, cfg.Targets, 1)
		assert.Equal(t, []string{TargetDrive}, cfg.Misconfigured)
	})

	t.Run("collision with a sibling internal token is refused", func(t *testing.T) {
		for _, sibling := range []string{
			siblingNotifyTokenEnv, siblingDocsNotifyTokenEnv,
			siblingBotMentionTokenEnv, siblingDriveInternalToken,
		} {
			cfg, problems := loadProvisioningConfig(env(map[string]string{
				envProvisionTargets:     "fleet",
				envProvisionFleetURL:    "https://fleet.internal/ensure",
				ProvisionFleetSecretEnv: okSecretA,
				sibling:                 okSecretA,
			}))
			require.Len(t, problems, 1, "sibling %s did not collide", sibling)
			assert.Empty(t, cfg.Targets)
			assert.Contains(t, problems[0].Error(), sibling)
			assert.NotContains(t, problems[0].Error(), okSecretA, "the error leaks the secret")
		}
	})

	t.Run("an unknown target name is refused", func(t *testing.T) {
		cfg, problems := loadProvisioningConfig(env(map[string]string{
			envProvisionTargets: "wukong",
		}))
		require.Len(t, problems, 1)
		assert.Empty(t, cfg.Targets)
		assert.Equal(t, []string{"wukong"}, cfg.Misconfigured)
	})

	t.Run("a malformed numeric knob falls back instead of disabling the control", func(t *testing.T) {
		cfg, _ := loadProvisioningConfig(env(map[string]string{
			envProvisionMaxAttempts: "12abc",
			envProvisionBatch:       "-1",
			envProvisionInterval:    "not-a-duration",
		}))
		assert.Equal(t, defaultProvisionMaxAttempts, cfg.MaxAttempts)
		assert.Equal(t, defaultProvisionBatch, cfg.BatchSize)
		assert.Equal(t, defaultProvisionInterval, cfg.Interval)
	})
}

// TestProvisioningRetryDelayHonoursItsCap pins the compute-then-clamp order. Clamping
// the exponent first would make 2^8 = 256s the real ceiling and shrink the documented
// retry budget by a sixth.
func TestProvisioningRetryDelayHonoursItsCap(t *testing.T) {
	assert.Equal(t, time.Second, provisioningRetryDelay(0))
	assert.Equal(t, 4*time.Second, provisioningRetryDelay(2))
	assert.Equal(t, 5*time.Minute, provisioningRetryDelay(9))
	assert.Equal(t, 5*time.Minute, provisioningRetryDelay(64))
}

// TestTruncateProvisioningErrorStaysWithinTheColumn covers the two failure modes that
// would otherwise break the backoff itself: an invalid byte makes strict mode reject
// the whole UPDATE, and a byte-boundary cut can leave half a rune behind.
func TestTruncateProvisioningErrorStaysWithinTheColumn(t *testing.T) {
	long := strings.Repeat("a", 252) + "😀"
	out := truncateProvisioningError(long)
	assert.LessOrEqual(t, len(out), 255)
	assert.True(t, utf8.ValidString(out), "truncation left an invalid UTF-8 tail: %q", out)

	dirty := "before\xff\xfeafter"
	assert.Equal(t, "beforeafter", truncateProvisioningError(dirty))
}
