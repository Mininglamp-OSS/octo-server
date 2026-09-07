package project

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	bodies["create"] = w.Body.String()
	second := decodeResp(t, w)
	// Drive the second project to ready as well, so its create response is compared
	// against ids that actually exist on the other side.
	p.processProvisioningJobs()
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

	// BOTH projects' ids. The first version recorded the create response of the SECOND
	// project into bodies["create"] but only iterated the FIRST project's rows, so a
	// create response echoing its own container id under a field name containing neither
	// "container" nor "provision" (workspace_id, slug) passed both assertions.
	var allRows []provRow
	allRows = append(allRows, readProvisioningRows(t, created.ProjectID)...)
	allRows = append(allRows, readProvisioningRows(t, second.ProjectID)...)
	require.Len(t, allRows, 4, "both projects should have one row per target")
	for name, body := range bodies {
		for _, row := range allRows {
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

// TestLastErrorCarriesTheTransportReasonAndSurvivesAbandon is the Q-2 fix end to end.
//
// A target that is down is the first failure an operator meets, and it used to write
// `transport_failed: projectprovision: transport_failed` — the outcome label twice — while
// the actual reason (refused / DNS / TLS / deadline) was captured and discarded. This is the
// field §3.4.1 of the runbook sends a human to read to decide whether to requeue, the sweep
// was deliberately changed to append to it because it is the only durable per-row evidence,
// and the client package has no logger by design, so an OOM-killed pod leaves nothing else.
//
// The second assertion is the half that would otherwise still be missing: finishProvisioning
// SETs last_error rather than appending, so the abandon path used to overwrite the detail
// with "retries exhausted" — on the one row that has no automatic re-drive and therefore
// needs the reason most.
func TestLastErrorCarriesTheTransportReasonAndSurvivesAbandon(t *testing.T) {
	// A bound-then-closed port: reachable and refusing, which is what a target whose Pod is
	// not up looks like. A stopped httptest server would do the same, but this cannot race
	// with the server's own shutdown.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	refusedURL := "http://" + ln.Addr().String() + "/api/internal/workspaces/ensure"
	require.NoError(t, ln.Close())

	_, p := setup(t)
	r := mountProject(t, p)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	p.nudgeProvisioningFn = func() {}
	p.cfg.Provisioning = ProvisioningConfig{
		Targets: []provisionTarget{{Target: projectprovision.Target{
			Name: TargetFleet, EnsureURL: refusedURL, Secret: provTestSecretFleet,
			Timeout: 2 * time.Second,
		}}},
		Interval: time.Hour, Timeout: 2 * time.Second, MaxAttempts: 2,
		BatchSize: defaultProvisionBatch,
	}

	created := createVia(t, r, token, "P")
	rows := readProvisioningRows(t, created.ProjectID)
	require.Len(t, rows, 1)
	id, containerID := rows[0].ID, rows[0].ContainerID

	p.processProvisioningJobs()
	rows = readProvisioningRows(t, created.ProjectID)
	require.Equal(t, provisionStatusPending, rows[0].Status)
	retryError := rows[0].LastError
	assert.Contains(t, retryError, "transport_failed", "the outcome label must still be there")
	assert.Contains(t, retryError, "refused",
		"last_error does not name WHY the call failed — this is the field the runbook sends a human to read")
	// Not the label twice: exactly one occurrence.
	assert.Equal(t, 1, strings.Count(retryError, "transport_failed"),
		"last_error restates its own outcome label, spending a 255-byte column on nothing: %q", retryError)
	assert.NotContains(t, retryError, containerID, "last_error leaks the container id")
	assert.NotContains(t, retryError, provTestSecretFleet, "last_error leaks the HMAC secret")
	assert.NotContains(t, retryError, refusedURL, "last_error carries the URL, so it took the wrapper not the inner error")

	// Second attempt exhausts the budget and abandons.
	makeProvisioningRowDue(t, id)
	p.processProvisioningJobs()
	rows = readProvisioningRows(t, created.ProjectID)
	require.Equal(t, provisionStatusAbandoned, rows[0].Status)
	assert.Contains(t, rows[0].LastError, "retries exhausted", "the give-up reason must be recorded")
	assert.Contains(t, rows[0].LastError, "refused",
		"the abandon write overwrote the transport reason — on the row with no automatic re-drive")
	assert.NotContains(t, rows[0].LastError, containerID)
	assert.LessOrEqual(t, len(rows[0].LastError), 255, "last_error must fit its column")
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

// TestSweepAbandonsACleanlyReleasedRowWhoseBudgetShrank is the P1-1 reproducer.
//
// releaseProvisioningJob NULLs lease_until on every retry, so the resting state of a
// retrying row is (pending, attempts = k, lease_until NULL). While the sweep required
// `lease_until IS NOT NULL` such a row was unsweepable, and once MaxAttempts was observed
// as m <= k it was unclaimable too — pending forever, no retry, no terminal state, no
// gauge, no log, and the only symptom a rising `pending` count that the runbook reads as
// "the target is not responding". Lowering the knob during an incident is the everyday
// route in; an out-of-range value casting to uint32(0) was the other, now refused at
// config load.
func TestSweepAbandonsACleanlyReleasedRowWhoseBudgetShrank(t *testing.T) {
	fleet := newFakeTarget(t)
	p, r, token := provisioningSetup(t, fleet)
	p.cfg.Provisioning.MaxAttempts = 8
	fleet.setStatus(http.StatusInternalServerError)

	created := createVia(t, r, token, "P")
	// One real failed attempt, so the row is released the way production releases it:
	// attempts advanced, lease_until back to NULL.
	p.processProvisioningJobs()
	rows := readProvisioningRows(t, created.ProjectID)
	require.Len(t, rows, 1)
	require.Equal(t, provisionStatusPending, rows[0].Status)
	require.Equal(t, uint32(1), rows[0].Attempts)
	require.Empty(t, rows[0].LeaseOwner)
	assertLeaseUntilIsNull(t, rows[0].ID)

	// The operator lowers the budget below what this row has already spent.
	p.cfg.Provisioning.MaxAttempts = 1

	// Claim can no longer take it (attempts < max is false) …
	p.processProvisioningJobs()
	rows = readProvisioningRows(t, created.ProjectID)
	require.Equal(t, uint32(1), rows[0].Attempts, "the row must not be claimable at the lowered budget")

	// … so the sweep is the only thing that can move it, and it must.
	p.sweepExhaustedProvisioningJobs()
	rows = readProvisioningRows(t, created.ProjectID)
	assert.Equal(t, provisionStatusAbandoned, rows[0].Status,
		"a cleanly released row past a lowered budget is neither claimable nor sweepable: "+
			"it sits pending forever with no signal")
	require.NotNil(t, rows[0].FinishedAt)
}

// assertLeaseUntilIsNull pins the precondition the reproducer above depends on — that a
// clean release really does clear the lease. If that ever changes the test would still
// pass while no longer testing the state it claims to.
func assertLeaseUntilIsNull(t *testing.T, id uint64) {
	t.Helper()
	var nullCount int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM `octo_project_provisioning` WHERE id = ? AND lease_until IS NULL", id,
	).LoadOne(&nullCount))
	require.Equal(t, 1, nullCount, "a released row must have lease_until = NULL")
}

// TestBothTargetsAdvanceInTheSameTick pins the index property that makes the per-target
// goroutines actually independent.
//
// Found by a flaky sibling test, not by review. Every claim/sweep/purge statement filters
// `target IN (...)`, but while the scan indexes led with `status` and not `target`, each
// per-target scan walked an index range containing the OTHER target's rows, locked them,
// and only then filtered them out at the server layer — and FOR UPDATE SKIP LOCKED makes
// the sibling scan skip a row a peer transaction is holding. Measured locally on MySQL
// 8.0.46: with two targets due in the same tick, the loser claimed NOTHING and waited
// silently for the next 15s tick. That is precisely the mutual starvation the one-goroutine-
// per-target fan-out exists to remove, so the fix is the index, not the fan-out.
//
// Both scan indexes now lead with (target, status), so this holds whichever one the
// optimizer picks.
//
// The property is inherently concurrent, so ONE tick is a probabilistic detector: measured
// against the defect reintroduced (both indexes led with `status`), a single tick caught it
// in 7 of 8 runs. That is a guard that reports green one time in eight while the defect is
// present, which is not a guard. Rounds is what makes it one: at ~0.875 per round, five
// independent rounds miss with probability ~3e-5. Rounds, not rows in one tick — a single
// wider tick correlates, five separate ticks do not.
func TestBothTargetsAdvanceInTheSameTick(t *testing.T) {
	fleet, drive := newFakeTarget(t), newFakeTarget(t)
	p, r, token := provisioningSetup(t, fleet, drive)
	// Both unhealthy, so the observable is `attempts`, which is written AT CLAIM: a row
	// still at 0 was never claimed, which is the starvation being pinned. A healthy target
	// would end at `ready` and hide whether the claim happened this tick or the next.
	fleet.setStatus(http.StatusInternalServerError)
	drive.setStatus(http.StatusInternalServerError)

	const rounds = 5
	for round := 0; round < rounds; round++ {
		created := createVia(t, r, token, fmt.Sprintf("P%d", round))
		// Exactly one tick per round. The round's two rows share status, next_attempt_at and
		// a NULL lease_until — one statement in one transaction enqueues them — so they sit
		// adjacent in the pending index range, which is what made the interleaving
		// reproducible. Earlier rounds' rows carry a backoff, so they are not due and this
		// tick sees only the pair just enqueued.
		p.processProvisioningJobs()

		attempts := map[string]uint32{}
		for _, row := range readProvisioningRows(t, created.ProjectID) {
			attempts[row.Target] = row.Attempts
		}
		require.Equal(t, uint32(1), attempts[TargetFleet],
			"round %d: fleet was not claimed in the tick that claimed drive — "+
				"one target's scan is locking the other target's rows", round)
		require.Equal(t, uint32(1), attempts[TargetDrive],
			"round %d: drive was not claimed in the tick that claimed fleet — "+
				"one target's scan is locking the other target's rows", round)
	}
}

// TestSweepSparesRowsParkedOnADisabledTarget pins the sweep's target filter, which has to
// match the claim path's.
//
// Turning a target off must be non-destructive: claimProvisioningJob filters `target IN ?`
// precisely so parked rows stop burning their retry budget against a destination nobody
// listens on and survive the switch being flipped back. Without the same filter on the
// sweep, the subset with attempts >= MaxAttempts went terminal `abandoned` anyway — a state
// with no automatic re-drive — so flipping the switch back would NOT resume them, which is
// the opposite of what the rollback runbook promises. Reachable by combining the documented
// rollback with the documented remedy of lowering OCTO_PROJECT_PROVISION_MAX_ATTEMPTS.
func TestSweepSparesRowsParkedOnADisabledTarget(t *testing.T) {
	fleet, drive := newFakeTarget(t), newFakeTarget(t)
	p, r, token := provisioningSetup(t, fleet, drive)
	p.cfg.Provisioning.MaxAttempts = 8
	fleet.setStatus(http.StatusInternalServerError)
	drive.setStatus(http.StatusInternalServerError)

	created := createVia(t, r, token, "P")
	// One real failed attempt each, so both rows rest the way production rests them:
	// pending, attempts advanced, lease_until NULL.
	p.processProvisioningJobs()
	rows := readProvisioningRows(t, created.ProjectID)
	require.Len(t, rows, 2)
	for _, row := range rows {
		require.Equal(t, provisionStatusPending, row.Status)
		require.Equal(t, uint32(1), row.Attempts)
	}

	// The operator narrows to fleet and lowers the budget below what both rows have spent —
	// the two documented moves, combined.
	p.cfg.Provisioning.Targets = []provisionTarget{fleetTargetOn(fleet)}
	p.cfg.Provisioning.MaxAttempts = 1
	p.sweepExhaustedProvisioningJobs()

	byTarget := map[string]uint8{}
	for _, row := range readProvisioningRows(t, created.ProjectID) {
		byTarget[row.Target] = row.Status
	}
	assert.Equal(t, provisionStatusAbandoned, byTarget[TargetFleet],
		"an exhausted row on an ENABLED target must still reach a terminal state")
	assert.Equal(t, provisionStatusPending, byTarget[TargetDrive],
		"the sweep drove a row on a DISABLED target to terminal abandoned: "+
			"flipping the target back on can no longer resume it")
}

// TestSweepLeavesANeverClaimedRowAlone is the other side of relaxing that predicate.
//
// The NULL arm is only safe because max >= 1 is guaranteed at config load: a never-claimed
// row has attempts = 0, so `attempts >= max` excludes it. This is the test that would fail
// if someone reintroduced a path to max = 0.
func TestSweepLeavesANeverClaimedRowAlone(t *testing.T) {
	fleet := newFakeTarget(t)
	p, r, token := provisioningSetup(t, fleet)
	created := createVia(t, r, token, "P")
	rows := readProvisioningRows(t, created.ProjectID)
	require.Len(t, rows, 1)
	require.Equal(t, uint32(0), rows[0].Attempts)
	assertLeaseUntilIsNull(t, rows[0].ID)

	p.sweepExhaustedProvisioningJobs()
	rows = readProvisioningRows(t, created.ProjectID)
	assert.Equal(t, provisionStatusPending, rows[0].Status,
		"a fresh, never-claimed row was swept to abandoned")
}

// TestWorkerAbandonsAPermanentFailureImmediately pins the short circuit.
//
// A container id mismatch means our mapping row points at a container nobody owns. The
// budget used to be spent on it anyway — a dozen more calls over ~23 minutes to reach the
// same terminal state, while re-hitting a target that already gave the answer.
func TestWorkerAbandonsAPermanentFailureImmediately(t *testing.T) {
	fleet := newFakeTarget(t)
	fleet.respondWith = "octows-somethingelse"
	p, r, token := provisioningSetup(t, fleet)

	before := testutil.ToFloat64(provisioningAttempts.WithLabelValues(TargetFleet, "container_id_mismatch"))
	created := createVia(t, r, token, "P")
	p.processProvisioningJobs()

	rows := readProvisioningRows(t, created.ProjectID)
	require.Len(t, rows, 1)
	assert.Equal(t, provisionStatusAbandoned, rows[0].Status)
	assert.Equal(t, uint32(1), rows[0].Attempts, "a permanent failure must not spend the budget")
	assert.Contains(t, rows[0].LastError, "permanent failure")
	assert.Equal(t, before+1,
		testutil.ToFloat64(provisioningAttempts.WithLabelValues(TargetFleet, "container_id_mismatch")))

	// And it is not re-called: abandoned is terminal and not claimable.
	calls, _ := fleet.snapshot()
	assert.Equal(t, 1, calls, "a permanent failure was retried against the target")
}

// TestWorkerKeepsRetryingAMissingEnsureEndpoint is the counterpart, and it is why the
// permanent set is small: a 404 is what a target that has not deployed its ensure endpoint
// looks like — the expected state until precondition P-2 lands — and it becomes a 200 after
// a deployment on the other side with nothing changing here. Abandoning it would turn a
// self-healing situation into one needing a manual requeue.
func TestWorkerKeepsRetryingAMissingEnsureEndpoint(t *testing.T) {
	fleet := newFakeTarget(t)
	fleet.setStatus(http.StatusNotFound)
	p, r, token := provisioningSetup(t, fleet)

	created := createVia(t, r, token, "P")
	p.processProvisioningJobs()

	rows := readProvisioningRows(t, created.ProjectID)
	require.Len(t, rows, 1)
	assert.Equal(t, provisionStatusPending, rows[0].Status,
		"a 404 must be retried: it is what a not-yet-deployed ensure endpoint looks like")
	assert.Contains(t, rows[0].LastError, "target_no_ensure_endpoint")
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

// TestTerminalWriteRequiresTheLease covers a hole a reviewer found by mutation: deleting
// `AND lease_owner = ?` from finishProvisioningJob left the whole suite GREEN, because no
// test ever drove an owner hand-over.
//
// That clause is what makes a stale executor's terminal write land on nothing after its
// lease expired and another replica took over. Without it, a slow worker finishing late
// would overwrite the state of whoever is running now.
func TestTerminalWriteRequiresTheLease(t *testing.T) {
	fleet := newFakeTarget(t)
	p, r, token := provisioningSetup(t, fleet)
	created := createVia(t, r, token, "P")
	rows := readProvisioningRows(t, created.ProjectID)
	require.Len(t, rows, 1)

	// Two claims of the same row: the second only becomes possible after the first lease
	// expires, so force that state directly rather than waiting two minutes.
	firstOwner := newProvisioningClaimOwner()
	job, err := p.db.claimProvisioningJob(firstOwner, []string{TargetFleet}, p.cfg.Provisioning.MaxAttempts, time.Now().UTC())
	require.NoError(t, err)
	require.NotNil(t, job)
	expireProvisioningLease(t, job.ID)
	secondOwner := newProvisioningClaimOwner()
	takenOver, err := p.db.claimProvisioningJob(secondOwner, []string{TargetFleet}, p.cfg.Provisioning.MaxAttempts, time.Now().UTC())
	require.NoError(t, err)
	require.NotNil(t, takenOver, "the expired lease should have been re-claimable")
	require.Equal(t, job.ID, takenOver.ID)

	// The FIRST owner now finishes, late. It must not land.
	ok, err := p.db.finishProvisioningJob(job.ID, firstOwner, provisionStatusReady, "", time.Now().UTC())
	require.NoError(t, err)
	assert.False(t, ok, "a stale owner's terminal write must not land: the lease changed hands")
	rows = readProvisioningRows(t, created.ProjectID)
	assert.Equal(t, provisionStatusPending, rows[0].Status,
		"the stale write set a terminal status underneath the current executor")
	assert.Equal(t, secondOwner, rows[0].LeaseOwner, "the stale write cleared the live lease")

	// The current owner's write does land.
	ok, err = p.db.finishProvisioningJob(job.ID, secondOwner, provisionStatusReady, "", time.Now().UTC())
	require.NoError(t, err)
	assert.True(t, ok)

	// Release is fenced the same way, and reports it rather than failing silently.
	err = p.db.releaseProvisioningJob(job.ID, firstOwner, 1, "late", time.Now().UTC())
	assert.ErrorIs(t, err, errProvisioningLeaseLost)
}

// expireProvisioningLease pushes a held lease into the past so the row is re-claimable
// without waiting out provisioningLease.
func expireProvisioningLease(t *testing.T, id uint64) {
	t.Helper()
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE `octo_project_provisioning` SET lease_until = ? WHERE id = ?",
		time.Now().UTC().Add(-time.Minute), id,
	).Exec()
	require.NoError(t, err)
}

// TestSweepSparesALeaseExpiredWithinTheGracePeriod is the second hole the same mutation
// pass found: reverting the grace from `now - lease` to `now` also left the suite GREEN,
// because both existing sweep fixtures sit OUTSIDE the window (one long-expired, one in
// the future). Nothing covered the middle, which is the only region the grace exists for.
//
// A job on its final attempt legitimately runs past its lease — the grace is what stops
// the sweep writing `abandoned` under it and leaving its own finish to land on nothing.
func TestSweepSparesALeaseExpiredWithinTheGracePeriod(t *testing.T) {
	fleet := newFakeTarget(t)
	p, r, token := provisioningSetup(t, fleet)
	p.cfg.Provisioning.MaxAttempts = 1
	created := createVia(t, r, token, "P")
	rows := readProvisioningRows(t, created.ProjectID)
	require.Len(t, rows, 1)

	// Budget spent, lease expired 30s ago — i.e. inside the one-lease grace. A running
	// final attempt looks exactly like this.
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE `octo_project_provisioning` SET attempts = ?, lease_owner = 'running', lease_until = ? WHERE id = ?",
		p.cfg.Provisioning.MaxAttempts, time.Now().UTC().Add(-30*time.Second), rows[0].ID,
	).Exec()
	require.NoError(t, err)

	p.sweepExhaustedProvisioningJobs()
	rows = readProvisioningRows(t, created.ProjectID)
	assert.Equal(t, provisionStatusPending, rows[0].Status,
		"a lease expired only 30s ago is inside the grace period: the executor is probably still running")

	// Past a full lease it is swept — otherwise this test would pass with no grace at all.
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE `octo_project_provisioning` SET lease_until = ? WHERE id = ?",
		time.Now().UTC().Add(-2*provisioningLease), rows[0].ID,
	).Exec()
	require.NoError(t, err)
	p.sweepExhaustedProvisioningJobs()
	rows = readProvisioningRows(t, created.ProjectID)
	assert.Equal(t, provisionStatusAbandoned, rows[0].Status)
	// And the release-path evidence survives the sweep: overwriting last_error with a
	// constant destroyed the only durable record of WHY, on the very path where there is
	// no log line either.
	assert.Contains(t, rows[0].LastError, "no executor released this row")
}

// TestPurgeDrainsRatherThanDeletingOneBatchPerTick pins the drain loop.
//
// A fixed cap per tick that is below the arrival rate is not a slower purge, it is no
// purge — the table grows without bound and every scan over it gets slower. The brief
// listed the drain as one of "P1's three corrections" while the code did one bounded
// DELETE per hourly tick.
func TestPurgeDrainsRatherThanDeletingOneBatchPerTick(t *testing.T) {
	_, p := setup(t)
	old := time.Now().UTC().Add(-2 * provisioningRetention)
	// More rows than one batch, so a single DELETE cannot finish the job.
	const rows = provisioningPurgeLimit + 250
	for i := 0; i < rows; i++ {
		containerID, err := newContainerID(TargetFleet)
		require.NoError(t, err)
		_, err = testCtx.DB().InsertBySql(
			"INSERT INTO `octo_project_provisioning` "+
				"(project_id, space_id, target, container_id, status, next_attempt_at, created_at, finished_at) "+
				"VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			fmt.Sprintf("purge-%d", i), spaceA, TargetFleet, containerID,
			provisionStatusDisbandPending, old, old, old,
		).Exec()
		require.NoError(t, err)
	}
	require.Equal(t, rows, countAllProvisioningRows(t))

	// The purge is gated on a declared reclaim consumer (see
	// TestPurgeIsGatedOnAReclaimConsumer); this test is about the drain loop, not the gate.
	p.cfg.Provisioning.ReclaimTargets = []string{TargetFleet}
	p.purgeProvisioningJobs()
	assert.Zero(t, countAllProvisioningRows(t),
		"one tick must drain the eligible set, not delete a single batch and leave the rest")
}

// TestPurgeIsGatedOnAReclaimConsumer pins the one irreversible operation in this slice
// against an assumption.
//
// The retention window is justified as a reclaim window, but the endpoint a consumer would
// poll does not exist in this repository yet. Purge before it ships and the status answer
// becomes permanently `unknown`, which D9 makes indistinguishable from "outside your
// grant" — so the container can never be reclaimed. Retained rows cost storage; that costs
// a leak.
func TestPurgeIsGatedOnAReclaimConsumer(t *testing.T) {
	_, p := setup(t)
	old := time.Now().UTC().Add(-2 * provisioningRetention)
	containerID, err := newContainerID(TargetFleet)
	require.NoError(t, err)
	_, err = testCtx.DB().InsertBySql(
		"INSERT INTO `octo_project_provisioning` "+
			"(project_id, space_id, target, container_id, status, next_attempt_at, created_at, finished_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		"p-gated", spaceA, TargetFleet, containerID,
		provisionStatusDisbandPending, old, old, old,
	).Exec()
	require.NoError(t, err)

	// Default: no consumer declared for any target, so nothing is deleted however old the
	// row is.
	require.Empty(t, p.cfg.Provisioning.ReclaimTargets)
	p.purgeProvisioningJobs()
	assert.Equal(t, 1, countAllProvisioningRows(t),
		"a purge ran with no reclaim consumer: the container becomes permanently un-reclaimable")

	// Declared for the OTHER target only: still nothing, because deletion authority is per
	// subsystem. This is the assertion the single-target fixtures could not make.
	p.cfg.Provisioning.ReclaimTargets = []string{TargetDrive}
	p.purgeProvisioningJobs()
	assert.Equal(t, 1, countAllProvisioningRows(t),
		"drive declaring a consumer deleted a fleet row: one subsystem authorized forgetting another's")

	// Declared for this target: the purge proceeds.
	p.cfg.Provisioning.ReclaimTargets = []string{TargetFleet}
	p.purgeProvisioningJobs()
	assert.Zero(t, countAllProvisioningRows(t))
}

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
	deleted, err := testDB.purgeFinishedProvisioningJobs(
		[]string{TargetFleet}, time.Now().UTC().Add(-provisioningRetention), 100)
	require.NoError(t, err)
	assert.Equal(t, int64(1), deleted)
	// Cardinality alone is vacuous here: a purge that deleted the `ready` row instead also
	// leaves 1 deleted and 2 survivors. Assert WHICH statuses survived.
	assert.ElementsMatch(t,
		[]int{int(provisionStatusReady), int(provisionStatusAbandoned)},
		survivingProvisioningStatuses(t),
		"the purge deleted a status it must never touch")
}

// survivingProvisioningStatuses reads back the statuses still in the table, so a purge
// assertion can name the rows rather than count them.
//
// []int, not []uint8: dbr scans a *[]uint8 as a byte slice, so the statuses came back as
// their ASCII codes and the comparison silently compared the wrong thing.
func survivingProvisioningStatuses(t *testing.T) []int {
	t.Helper()
	var statuses []int
	_, err := testCtx.DB().SelectBySql("SELECT status FROM `octo_project_provisioning`").Load(&statuses)
	require.NoError(t, err)
	return statuses
}

// TestPurgeSparesRowsOfTargetsWithNoDeclaredReclaimConsumer is the mixed-target case the
// single-target fixtures could not express.
//
// A live consumer is a PER-SUBSYSTEM deliverable — fleet and drive ship on different
// teams' schedules, which is why everything else about a target here is per target. With a
// process-global gate and a DELETE carrying no target predicate, fleet shipping its
// consumer first silently authorized deleting drive's reclaim records too, 90 days later
// and with no gauge for it: the D9 answer for those containers becomes permanently
// `unknown`, which is indistinguishable from "outside your grant", so they can never be
// reclaimed. Deletion is the only irreversible operation in this slice.
func TestPurgeSparesRowsOfTargetsWithNoDeclaredReclaimConsumer(t *testing.T) {
	_, p := setup(t)
	old := time.Now().UTC().Add(-2 * provisioningRetention)
	for i, target := range []string{TargetFleet, TargetDrive} {
		containerID, err := newContainerID(target)
		require.NoError(t, err)
		_, err = testCtx.DB().InsertBySql(
			"INSERT INTO `octo_project_provisioning` "+
				"(project_id, space_id, target, container_id, status, next_attempt_at, created_at, finished_at) "+
				"VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			fmt.Sprintf("mixed-%d", i), spaceA, target, containerID,
			provisionStatusDisbandPending, old, old, old,
		).Exec()
		require.NoError(t, err)
	}
	require.Equal(t, 2, countAllProvisioningRows(t))

	// Only fleet has a consumer. Drive's row is equally old and equally eligible by every
	// other predicate, so it isolates the target clause.
	p.cfg.Provisioning.ReclaimTargets = []string{TargetFleet}
	p.purgeProvisioningJobs()

	remaining := readProvisioningTargets(t)
	assert.Equal(t, []string{TargetDrive}, remaining,
		"the purge deleted reclaim records for a target whose consumer nobody declared live")
}

// readProvisioningTargets lists the targets still present, so a purge assertion can name
// whose rows survived rather than count them.
func readProvisioningTargets(t *testing.T) []string {
	t.Helper()
	var targets []string
	_, err := testCtx.DB().SelectBySql(
		"SELECT target FROM `octo_project_provisioning` ORDER BY target").Load(&targets)
	require.NoError(t, err)
	return targets
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

	// pending and abandoned rows COUNT, because under at-least-once delivery a lost
	// response leaves a container that exists behind a row that does not say so. The first
	// version counted `ready` only, which contradicted what this module says elsewhere
	// about abandoned rows ("we may well have created one and failed to record it") and
	// made a security-surface gauge under-report.
	insertProvisioningRowFixture(t, "p-pending", TargetDrive, provisionStatusPending)
	insertProvisioningRowFixture(t, "p-abandoned", TargetDrive, provisionStatusAbandoned)
	insertProvisioningRowFixture(t, "p-disbanded", TargetDrive, provisionStatusDisbandPending)
	p.refreshProvisioningMetrics()
	assert.Equal(t, 3.0, testutil.ToFloat64(provisioningUnnarrowedContainers.WithLabelValues(TargetDrive)),
		"ready + pending + abandoned must all count as possibly-existing containers")

	// disband_pending is excluded deliberately, not by the same oversight: those are
	// explicitly enumerated for reclaim and stay visible in their own row bucket.
	assert.Equal(t, 1.0, testutil.ToFloat64(provisioningRows.WithLabelValues(TargetDrive, "disband_pending")))

	// A ROLLBACK must not make the exposure read zero. Clearing
	// OCTO_PROJECT_PROVISION_TARGETS stops us talking to the target; it does not
	// un-provision anything, and narrowing is a property of the subsystem rather than of
	// whether we are currently configured for it.
	p.cfg.Provisioning.Targets = nil
	p.refreshProvisioningMetrics()
	assert.Equal(t, 3.0, testutil.ToFloat64(provisioningUnnarrowedContainers.WithLabelValues(TargetDrive)),
		"clearing the target list made the exposed surface read 0 while the containers still exist")
	// The whole census must survive a rollback too. It used to be scheduled inside
	// startProvisioningWorker, behind the same early return, so clearing the target list
	// took every gauge with it on the next deploy — including the `pending` backlog the
	// runbook promises is preserved. Rollback is exactly when someone is reading these.
	assert.Equal(t, 1.0, testutil.ToFloat64(provisioningRows.WithLabelValues(TargetDrive, "pending")),
		"the row census vanished for a target that is no longer enabled")

	// It does fall to 0 once the rows are actually gone — a gauge that holds a stale
	// reading after the condition clears reads as an unresolved alert.
	_, err := testCtx.DB().DeleteBySql("DELETE FROM `octo_project_provisioning`").Exec()
	require.NoError(t, err)
	p.refreshProvisioningMetrics()
	assert.Equal(t, 0.0, testutil.ToFloat64(provisioningUnnarrowedContainers.WithLabelValues(TargetDrive)))
	assert.Equal(t, 0.0, testutil.ToFloat64(provisioningRows.WithLabelValues(TargetDrive, "ready")))
}

// insertProvisioningRowFixture writes one row in a given terminal/pending state.
func insertProvisioningRowFixture(t *testing.T, projectID, target string, status uint8) {
	t.Helper()
	now := time.Now().UTC()
	containerID, err := newContainerID(target)
	require.NoError(t, err)
	_, err = testCtx.DB().InsertBySql(
		"INSERT INTO `octo_project_provisioning` "+
			"(project_id, space_id, target, container_id, status, next_attempt_at, created_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?)",
		projectID, spaceA, target, containerID, status, now, now,
	).Exec()
	require.NoError(t, err)
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

	// The reclaim switches are the gate on the one irreversible statement in the slice, and
	// every other assertion about them sets the struct field directly — so nothing pinned
	// that the env NAMES are read at all. A typo in either constant fails safe (the purge
	// stays off) but silently, and silently-off is exactly the state an operator would
	// believe they had left.
	//
	// The env names are spelled out as LITERALS here, not referenced through the constants.
	// Keying the fixture with the same constant the code reads is a tautology: renaming or
	// mistyping the constant renames it on both sides and the test stays green — measured,
	// by mutating the drive constant to a name ending in _LIV and watching this pass. These
	// literals are the deployed contract (a configmap key, not a Go identifier), so a
	// literal is what the assertion has to be made of.
	t.Run("reclaim consumers resolve per target from their own envs", func(t *testing.T) {
		cfg, problems := loadProvisioningConfig(env(map[string]string{
			"OCTO_PROJECT_PROVISION_DRIVE_RECLAIM_CONSUMER_LIVE": "true",
		}))
		require.Empty(t, problems)
		assert.Equal(t, []string{TargetDrive}, cfg.ReclaimTargets,
			"the per-target reclaim env did not resolve, or resolved for the wrong target")

		both, problems := loadProvisioningConfig(env(map[string]string{
			"OCTO_PROJECT_PROVISION_FLEET_RECLAIM_CONSUMER_LIVE": "true",
			"OCTO_PROJECT_PROVISION_DRIVE_RECLAIM_CONSUMER_LIVE": "true",
		}))
		require.Empty(t, problems)
		assert.Equal(t, []string{TargetFleet, TargetDrive}, both.ReclaimTargets)
	})

	// Read WITHOUT any target enabled on purpose: a target switched off after use still has
	// containers whose reclaim accounting its live consumer must be able to finish, so this
	// has to resolve past the empty-target-list early return.
	t.Run("reclaim consumers resolve with no target enabled", func(t *testing.T) {
		cfg, problems := loadProvisioningConfig(env(map[string]string{
			"OCTO_PROJECT_PROVISION_FLEET_RECLAIM_CONSUMER_LIVE": "true",
		}))
		require.Empty(t, problems)
		require.False(t, cfg.Enabled())
		assert.Equal(t, []string{TargetFleet}, cfg.ReclaimTargets)
	})

	t.Run("the retired process-global reclaim env is refused, not ignored", func(t *testing.T) {
		cfg, problems := loadProvisioningConfig(env(map[string]string{
			"OCTO_PROJECT_PROVISION_RECLAIM_CONSUMER_LIVE": "true",
		}))
		require.Len(t, problems, 1)
		assert.Contains(t, problems[0].Error(), "OCTO_PROJECT_PROVISION_RECLAIM_CONSUMER_LIVE")
		assert.Empty(t, cfg.ReclaimTargets,
			"the retired global switch must not authorize purging any target")
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
		cfg, problems := loadProvisioningConfig(env(map[string]string{
			envProvisionMaxAttempts: "12abc",
			envProvisionBatch:       "-1",
			envProvisionInterval:    "not-a-duration",
		}))
		assert.Equal(t, defaultProvisionMaxAttempts, cfg.MaxAttempts)
		assert.Equal(t, defaultProvisionBatch, cfg.BatchSize)
		assert.Equal(t, defaultProvisionInterval, cfg.Interval)
		// Falling back is right; falling back SILENTLY is what made the uint32 wrap
		// below invisible, so a bad MaxAttempts must also be reported.
		assert.NotEmpty(t, problems, "a rejected MaxAttempts must be reported, not only defaulted")
	})

	t.Run("MaxAttempts out of range is refused rather than cast into a no-op", func(t *testing.T) {
		// 4294967296 = 2^32. It parses as a positive int and casts to uint32(0), which
		// makes every claim predicate `attempts < 0` and turns the whole slice into a
		// silent no-op from the first tick — with no signal anywhere.
		for _, raw := range []string{"4294967296", "0", "-3", strconv.Itoa(maxProvisionAttemptsCeiling + 1)} {
			cfg, problems := loadProvisioningConfig(env(map[string]string{envProvisionMaxAttempts: raw}))
			assert.Equal(t, defaultProvisionMaxAttempts, cfg.MaxAttempts, "raw=%q", raw)
			require.NotEmpty(t, problems, "raw=%q was accepted silently", raw)
			assert.Contains(t, problems[0].Error(), envProvisionMaxAttempts)
		}
		// And a legitimate value inside the range still applies.
		cfg, problems := loadProvisioningConfig(env(map[string]string{envProvisionMaxAttempts: "5"}))
		assert.Equal(t, uint32(5), cfg.MaxAttempts)
		assert.Empty(t, problems)
	})

	t.Run("a Timeout that could outlive the lease is refused", func(t *testing.T) {
		// With no heartbeat, a call that outlives its lease gets a second executor on the
		// same row and can be marked abandoned while it is still running. The relationship
		// used to be only a comment.
		for _, raw := range []string{"10m", "2m", "1m", "0s", "nonsense"} {
			cfg, problems := loadProvisioningConfig(env(map[string]string{envProvisionTimeout: raw}))
			assert.Equal(t, defaultProvisionTimeout, cfg.Timeout, "raw=%q", raw)
			require.NotEmpty(t, problems, "raw=%q was accepted", raw)
			assert.Contains(t, problems[0].Error(), envProvisionTimeout)
		}
		cfg, problems := loadProvisioningConfig(env(map[string]string{envProvisionTimeout: "20s"}))
		assert.Equal(t, 20*time.Second, cfg.Timeout)
		assert.Empty(t, problems)
		// The default must itself satisfy the constraint, or the constraint is theatre.
		assert.LessOrEqual(t, int64(defaultProvisionTimeout), int64(provisioningLease/maxProvisionTimeoutFraction))
	})

	t.Run("knob problems are reported even with no target enabled", func(t *testing.T) {
		// Otherwise the misconfiguration is discovered on the day someone turns a target
		// on, which is the worst possible moment.
		cfg, problems := loadProvisioningConfig(env(map[string]string{envProvisionMaxAttempts: "0"}))
		assert.False(t, cfg.Enabled())
		assert.NotEmpty(t, problems)
		assert.NotEmpty(t, cfg.Problems, "New() logs from cfg.Problems, so it must carry them too")
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
