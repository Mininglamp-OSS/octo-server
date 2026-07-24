package notify

import (
	"net/http"
	"testing"
	"time"

	libcommon "github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// liveWuKongIMReachable probes <APIURL>/health so the e2e SKIPS (not fails) when
// no live WuKongIM is running. Checking APIURL != "" is not enough: config.New()
// defaults APIURL to http://127.0.0.1:5001, so the empty-check never triggers and
// the package would fail hard on a dev box without a live IM.
func liveWuKongIMReachable(apiURL string) bool {
	if apiURL == "" {
		return false
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(apiURL + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// TestGroupWelcome_E2E_LiveCoalescedPost drives the FULL group-welcome pipeline
// against a live WuKongIM (the real sender is NOT stubbed): config + two first-join
// members -> event enqueue -> coalesce window -> batch claim -> ONE public post to
// the group channel -> ledger SENT. It also answers the brief's open question end
// to end — the fixed `notification` sender posts into a group channel it is not a
// member of — and proves coalescing by asserting both joiners share one message_id.
//
// Skips (does not fail) when no live WuKongIM is configured, so unit-only runs stay
// green; CI (which provisions WuKongIM) exercises the real wire.
func TestGroupWelcome_E2E_LiveCoalescedPost(t *testing.T) {
	ctx := swTestServer(t)
	if !liveWuKongIMReachable(ctx.GetConfig().WuKongIM.APIURL) {
		t.Skip("no live WuKongIM reachable at APIURL/health — skipping live e2e")
	}

	const groupNo = "g_e2e"
	af := time.Now().UTC().Add(-time.Hour)
	gwInsertGroup(t, ctx, groupNo, 1)
	// Create the group's IM channel with members that do NOT include notification,
	// so the post exercises "sender is not a group member".
	require.NoError(t, ctx.IMCreateOrUpdateChannel(&config.ChannelCreateReq{
		ChannelID:   groupNo,
		ChannelType: libcommon.ChannelTypeGroup.Uint8(),
		Subscribers: []string{"e2e_a", "e2e_b"},
	}))
	gwInsertUserNamed(t, ctx, "e2e_a", "Alice", 0)
	gwInsertUserNamed(t, ctx, "e2e_b", "Bob", 0)
	gwInsertGroupMember(t, ctx, groupNo, "e2e_a", 0, 1, time.Now().UTC())
	gwInsertGroupMember(t, ctx, groupNo, "e2e_b", 0, 1, time.Now().UTC())
	gwSetGroupConfig(t, ctx, groupNo, true, af, "欢迎 {member} 入群")

	svc := gwNewService(t, ctx) // master switch ON; real sender (sendFn NOT stubbed)

	// Event path enqueues both members; advance the clock past the coalesce window
	// so the worker batch-claims them together.
	clock := time.Now().UTC()
	svc.now = func() time.Time { return clock }
	svc.handleMemberJoin(groupNo, "e2e_a")
	svc.handleMemberJoin(groupNo, "e2e_b")
	ids := gwPendingIDs(t, ctx, groupNo)
	require.Len(t, ids, 2, "both first-join members enqueued")
	clock = clock.Add(time.Minute)

	svc.runWorkerWake(bg())

	sent, _ := svc.store.countByStatus(bg(), groupNo, swStatusSent)
	assert.EqualValues(t, 2, sent, "both joiners delivered to the live group channel")

	// Coalescing: both rows carry the SAME non-zero message_id => ONE public post.
	_, midA := gwRowStatus(t, ctx, ids[0])
	_, midB := gwRowStatus(t, ctx, ids[1])
	assert.NotZero(t, midA, "live WuKongIM returned a message_id")
	assert.Equal(t, midA, midB, "both joiners share one message => a single coalesced post")
}

// gwPendingIDs returns the ledger row ids for groupNo ordered by id (ascending).
func gwPendingIDs(t *testing.T, ctx *config.Context, groupNo string) []int64 {
	t.Helper()
	var ids []int64
	_, err := ctx.DB().SelectBySql(
		"SELECT id FROM "+groupWelcomeTable+" WHERE group_no=? ORDER BY id", groupNo,
	).Load(&ids)
	require.NoError(t, err)
	return ids
}
