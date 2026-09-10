package project

// 收敛动作用尽页数预算时的答案，以及它为什么与级联**不同**。
//
// 第十一轮 review 的 P2-3：上一版这里返回 errCascadeIncomplete，注释还写着"与级联
// 同一条可重试错误"。那句话拆开看就是假的，而且假在让重试有意义的那一半——
//
//   级联查活跃席位，关掉的行会离开它自己的结果集，所以每次重试从更小的集合开始；
//   收敛动作故意不带 status 过滤（带了就返回空集、什么都收敛不了），于是重试从
//   project_id > '' 重新走同样的前 N 行，永远走不到第 N+1 行，直到工单被判 abandoned。
//
// 所以两个用例是成对的：级联那半（TestCascadeReturnsRetryableErrorWhenBudgetExhausted）
// 断言"必须要求重试"，这半断言"必须不要求"。把任何一个的答案抄给另一个都是缺陷。

import (
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubOwnerSyncRecorder replaces ONLY the owner-transfer hook, recording which projects
// the convergence asked about, and puts the real one back afterwards.
//
// Only that hook: the projects in these cases are created through the real HTTP path, so
// the real provisioner has to stay live or queryAllMemberGroupNo answers "no group" and
// the convergence correctly does nothing — a stand-in disagreeing with production about
// the thing under test. The registry is process-wide and latest-wins, so leaving a
// stand-in behind silently disables the feature for every later case; that is the
// order-dependent failure -shuffle=on exists to surface.
func stubOwnerSyncRecorder(t *testing.T, into *[]string) func() {
	t.Helper()
	snapshot := SnapshotAllMemberGroupHooksForTest()
	RegisterAllMemberGroupOwnerTransfer(func(_ *config.Context, projectID, _ string) error {
		*into = append(*into, projectID)
		return nil
	})
	return func() { RestoreAllMemberGroupHooksForTest(snapshot) }
}

func runOwnerConvergence(t *testing.T, p *Project, spaceID, uid string) error {
	t.Helper()
	return p.convergeAllMemberGroupOwners(testCtx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: uid, OperatorUID: "owner1",
		Reason: spacemod.MemberRemoveReasonKicked,
	})
}

// TestOwnerConvergenceDoesNotAskForARetryItCannotWin pins the P2-3 fix.
func TestOwnerConvergenceDoesNotAskForARetryItCannotWin(t *testing.T) {
	srv, p := setup(t)
	seedSpace(t, spaceA, 1)
	ownerTok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	seedUser(t, "wide")
	seedSpaceMember(t, spaceA, "wide", 0, 1)

	origPage, origMax := cascadePageSize, cascadeMaxPages
	cascadePageSize, cascadeMaxPages = 1, 2
	t.Cleanup(func() { cascadePageSize, cascadeMaxPages = origPage, origMax })

	for i := 0; i < 4; i++ {
		proj := createProjectVia(t, srv, spaceA, ownerTok, "conv-"+string(rune('a'+i)))
		w := doJSON(t, srv, http.MethodPost, "/v1/projects/"+proj.ProjectID+"/members/add",
			ownerTok, map[string]any{"uids": []string{"wide"}})
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	}
	removeSpaceMember(t, spaceA, "wide")

	before := promtestutil.ToFloat64(allMemberGroupConvergenceIncomplete)

	require.NoError(t, runOwnerConvergence(t, p, spaceA, "wide"),
		"with the budget spent the finalizer must NOT ask for a retry. Its query has no "+
			"self-shrinking filter, so a retry re-reads the same first page forever and the "+
			"job burns its whole attempt budget on a walk that cannot advance — taking every "+
			"already-successful cleanup step through twenty pointless re-runs with it")

	assert.Equal(t, before+1, promtestutil.ToFloat64(allMemberGroupConvergenceIncomplete),
		"and it must not be silent: giving up on the remaining projects is a distinct, "+
			"countable event. Its own counter, not a kind on the sync-FAILURE counter — this "+
			"is not a failure, and folding it in would make any alert that sums that counter "+
			"page on a normal event")
}

// TestOwnerConvergenceStillWalksEveryPageWithinBudget is the control.
//
// Without it the case above passes just as well against a convergence that returns nil
// immediately and syncs nothing — which is the vacuity shape this task has been caught
// on repeatedly.
func TestOwnerConvergenceStillWalksEveryPageWithinBudget(t *testing.T) {
	srv, p := setup(t)
	seedSpace(t, spaceA, 1)
	ownerTok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	seedUser(t, "wide")
	seedSpaceMember(t, spaceA, "wide", 0, 1)

	origPage, origMax := cascadePageSize, cascadeMaxPages
	cascadePageSize, cascadeMaxPages = 1, 25
	t.Cleanup(func() { cascadePageSize, cascadeMaxPages = origPage, origMax })

	const projects = 4
	var syncedProjects []string
	restore := stubOwnerSyncRecorder(t, &syncedProjects)
	defer restore()

	for i := 0; i < projects; i++ {
		proj := createProjectVia(t, srv, spaceA, ownerTok, "walk-"+string(rune('a'+i)))
		w := doJSON(t, srv, http.MethodPost, "/v1/projects/"+proj.ProjectID+"/members/add",
			ownerTok, map[string]any{"uids": []string{"wide"}})
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	}
	removeSpaceMember(t, spaceA, "wide")

	syncedProjects = nil
	require.NoError(t, runOwnerConvergence(t, p, spaceA, "wide"))
	assert.Len(t, syncedProjects, projects,
		"a multi-page walk inside the budget must visit EVERY project the member had a row "+
			"in — page size is 1 here, so this fails if the walk stops after its first page")
}
