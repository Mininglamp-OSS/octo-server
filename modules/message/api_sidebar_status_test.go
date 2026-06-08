//go:build integration

package message

// =============================================================================
// Sidebar thread-status surfacing — DB-backed tests (GH octo-server#310)
//
// /sidebar/sync must stamp each thread item (target_type=5) with the thread
// lifecycle status (1=active / 2=archived / 3=deleted) so the client can filter
// archived threads synchronously. These tests seed real thread rows and drive
// the same helpers Sidebar.Sync uses:
//   - loadThreadLastMsgAt  (follow tab: last_message_at + status, one query)
//   - loadThreadStatuses   (recent tab: status only, one batched query)
//   - backfillThreadStatus (stamp Status onto thread items)
//
// Build-tagged `integration` (run with `go test -tags=integration`), matching
// api_sidebar_integration_test.go / api_sidebar_recent_filter_e2e_test.go:
// these spin up testutil.NewTestServer against the shared `test` DB, which is
// order-fragile under the default `go test ./...` job (a prior test in the
// package can leave the schema in a state where NewTestServer's module.Setup
// re-applies a migration whose table already exists and panics). The pure
// backfill / JSON-shape logic is covered by api_sidebar_test.go in the default
// job; this file is the extra DB-backed glue check.
// =============================================================================

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	convext "github.com/Mininglamp-OSS/octo-server/modules/conversation_ext"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedThread inserts a thread row with the given group/short/status.
func seedThread(t *testing.T, ctx *config.Context, groupNo, shortID string, status int, lastMsgAt *time.Time) {
	t.Helper()
	tdb := thread.NewDB(ctx)
	require.NoError(t, tdb.Insert(&thread.Model{
		ShortID:       shortID,
		GroupNo:       groupNo,
		Name:          "thr-" + shortID,
		CreatorUID:    "creator",
		Status:        status,
		Version:       1,
		LastMessageAt: lastMsgAt,
	}), "seed thread %s____%s status=%d", groupNo, shortID, status)
}

func threadChannelID(groupNo, shortID string) string { return groupNo + "____" + shortID }

// loadThreadLastMsgAt must surface the thread status alongside last_message_at,
// keyed by "{groupNo}____{shortID}", and must omit deleted threads (the
// underlying QueryActiveByGroupShortIDs filters status=deleted).
func TestLoadThreadLastMsgAt_SurfacesStatus(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)

	const g = "g310a"
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	seedThread(t, ctx, g, "act", thread.ThreadStatusActive, &at)
	seedThread(t, ctx, g, "arc", thread.ThreadStatusArchived, &at)
	seedThread(t, ctx, g, "del", thread.ThreadStatusDeleted, &at)

	sb := NewSidebar(ctx)
	extRows := []*convext.Model{
		{TargetID: threadChannelID(g, "act")},
		{TargetID: threadChannelID(g, "arc")},
		{TargetID: threadChannelID(g, "del")},
	}
	lastMsgAt, statusMap, err := sb.loadThreadLastMsgAt(extRows)
	require.NoError(t, err)

	assert.Equal(t, thread.ThreadStatusActive, statusMap[threadChannelID(g, "act")])
	assert.Equal(t, thread.ThreadStatusArchived, statusMap[threadChannelID(g, "arc")])
	_, delPresent := statusMap[threadChannelID(g, "del")]
	assert.False(t, delPresent, "deleted thread must be filtered out (not in status map)")

	// last_message_at map keeps the same keys (alive markers for mergeThreadEntries).
	_, actAlive := lastMsgAt[threadChannelID(g, "act")]
	_, arcAlive := lastMsgAt[threadChannelID(g, "arc")]
	assert.True(t, actAlive)
	assert.True(t, arcAlive)
	_, delAlive := lastMsgAt[threadChannelID(g, "del")]
	assert.False(t, delAlive, "deleted thread must not be an alive marker")
}

// loadThreadStatuses (recent-tab path) must batch-query status for thread items
// only, key by the composite "{groupNo}____{shortID}", and drop deleted threads.
// short_id is globally UNIQUE in the thread schema (uk_short_id), so a literal
// same-short_id-across-two-groups collision can't be persisted; the composite
// key still guarantees each thread item is matched to its own group's row (the
// same-short_id backfill collision is covered purely in
// TestBackfillThreadStatus_NoCrossGroupMismatch).
func TestLoadThreadStatuses_BatchedNoCrossGroup(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)

	const gA, gB = "g310bA", "g310bB"
	at := time.Now().Add(-time.Hour)
	seedThread(t, ctx, gA, "bA-act", thread.ThreadStatusActive, &at)
	seedThread(t, ctx, gB, "bB-arc", thread.ThreadStatusArchived, &at)
	seedThread(t, ctx, gA, "bA-gone", thread.ThreadStatusDeleted, &at)

	sb := NewSidebar(ctx)
	items := []*SidebarItem{
		{TargetType: int(common.ChannelTypePerson), TargetID: "peer1"},
		{TargetType: int(common.ChannelTypeGroup), TargetID: gA},
		{TargetType: int(common.ChannelTypeCommunityTopic), TargetID: threadChannelID(gA, "bA-act")},
		{TargetType: int(common.ChannelTypeCommunityTopic), TargetID: threadChannelID(gB, "bB-arc")},
		{TargetType: int(common.ChannelTypeCommunityTopic), TargetID: threadChannelID(gA, "bA-gone")},
	}
	statusMap, err := sb.loadThreadStatuses(items)
	require.NoError(t, err)

	assert.Equal(t, thread.ThreadStatusActive, statusMap[threadChannelID(gA, "bA-act")],
		"gA active thread matched to its own (group, short) row")
	assert.Equal(t, thread.ThreadStatusArchived, statusMap[threadChannelID(gB, "bB-arc")],
		"gB archived thread matched to its own (group, short) row")
	_, gonePresent := statusMap[threadChannelID(gA, "bA-gone")]
	assert.False(t, gonePresent, "deleted thread must not appear in status map")
}

// loadThreadStatuses with no thread items must not query and returns an empty map.
func TestLoadThreadStatuses_NoThreadItems(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)

	sb := NewSidebar(ctx)
	items := []*SidebarItem{
		{TargetType: int(common.ChannelTypePerson), TargetID: "peer1"},
		{TargetType: int(common.ChannelTypeGroup), TargetID: "g1"},
	}
	statusMap, err := sb.loadThreadStatuses(items)
	require.NoError(t, err)
	assert.Empty(t, statusMap)
}

// Follow tab end-to-end: both the IM-present thread (buildFollowItems) and the
// DB-only thread (mergeThreadEntries) must carry the correct Status after the
// backfill loop; a deleted thread on the DB-only path is excluded by
// mergeThreadEntries (it is absent from the active-thread/last_message_at map),
// so it never reaches the backfill loop.
func TestSidebar_FollowTab_BackfillsStatus(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)

	const g = "g310c"
	at := time.Now().Add(-time.Hour)
	seedThread(t, ctx, g, "imact", thread.ThreadStatusActive, &at)   // IM-present, active
	seedThread(t, ctx, g, "dbarc", thread.ThreadStatusArchived, &at) // DB-only, archived
	seedThread(t, ctx, g, "dbdel", thread.ThreadStatusDeleted, &at)  // DB-only, deleted → excluded

	imActID := threadChannelID(g, "imact")
	dbArcID := threadChannelID(g, "dbarc")
	dbDelID := threadChannelID(g, "dbdel")

	catID := "cat-310c"
	categorySetting := map[string]*GroupCategorySetting{
		g: {GroupNo: g, CategoryID: &catID, CategorySort: 1, CategoryGroupSort: 1},
	}
	threadExtRows := []*convext.Model{
		{TargetID: imActID},
		{TargetID: dbArcID},
		{TargetID: dbDelID},
	}
	threadExtMap := map[string]*convext.Model{}
	for _, m := range threadExtRows {
		threadExtMap[m.TargetID] = m
	}

	// IM returns only the active thread; archived + deleted are DB-only.
	stubConvs := []*config.SyncUserConversationResp{
		{ChannelID: imActID, ChannelType: common.ChannelTypeCommunityTopic.Uint8(), Timestamp: 100},
	}

	sb := NewSidebar(ctx)
	items := buildFollowItems(stubConvs, categorySetting, nil, nil, threadExtMap, nil, nil, nil, nil, "")

	lastMsgAt, statusMap, err := sb.loadThreadLastMsgAt(threadExtRows)
	require.NoError(t, err)
	items = mergeThreadEntries(items, threadExtRows, lastMsgAt, categorySetting, nil, nil, nil, "")
	backfillThreadStatus(items, statusMap)

	byID := map[string]*SidebarItem{}
	for _, it := range items {
		byID[it.TargetID] = it
	}
	require.Contains(t, byID, imActID, "IM-present active thread must be present")
	require.Contains(t, byID, dbArcID, "DB-only archived thread must be merged in")
	assert.NotContains(t, byID, dbDelID,
		"deleted thread (DB-only) must be excluded (mergeThreadEntries skips non-alive)")

	assert.Equal(t, thread.ThreadStatusActive, byID[imActID].Status,
		"IM-present path (buildFollowItems) thread must be stamped active")
	assert.Equal(t, thread.ThreadStatusArchived, byID[dbArcID].Status,
		"DB-only path (mergeThreadEntries) thread must be stamped archived")
}

// Recent tab end-to-end: thread items must be stamped with status from the
// single batched query; deleted threads carry no status (and the recent tab,
// unlike the conversation sync, does not itself drop them — that's the client's
// job using this very field).
func TestSidebar_RecentTab_BackfillsStatus(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)

	const g = "g310d"
	at := time.Now().Add(-time.Hour)
	seedThread(t, ctx, g, "ract", thread.ThreadStatusActive, &at)
	seedThread(t, ctx, g, "rarc", thread.ThreadStatusArchived, &at)

	actID := threadChannelID(g, "ract")
	arcID := threadChannelID(g, "rarc")

	// person window 0 keeps DMs; group/thread within window.
	cutoffs := recentCutoffs{group: 0, thread: 0, person: 0}
	stubConvs := []*config.SyncUserConversationResp{
		{ChannelID: "peer1", ChannelType: common.ChannelTypePerson.Uint8(), Timestamp: 100},
		{ChannelID: actID, ChannelType: common.ChannelTypeCommunityTopic.Uint8(), Timestamp: 200},
		{ChannelID: arcID, ChannelType: common.ChannelTypeCommunityTopic.Uint8(), Timestamp: 300},
	}

	sb := NewSidebar(ctx)
	items := buildRecentItems(stubConvs, cutoffs, nil, nil, nil, "")
	statusMap, err := sb.loadThreadStatuses(items)
	require.NoError(t, err)
	backfillThreadStatus(items, statusMap)

	byID := map[string]*SidebarItem{}
	for _, it := range items {
		byID[it.TargetID] = it
	}
	assert.Equal(t, thread.ThreadStatusActive, byID[actID].Status)
	assert.Equal(t, thread.ThreadStatusArchived, byID[arcID].Status)
	assert.Equal(t, 0, byID["peer1"].Status, "DM must keep Status=0 (omitempty)")
}

// Recent tab fail-open: when the thread status query errors, the handler logs a
// warning and leaves Status unset — it must NOT fail the request. We reproduce
// the handler's recent branch against a context whose schema lacks the thread
// table, so QueryActiveByGroupShortIDs errors, and assert items survive with
// Status=0.
func TestSidebar_RecentTab_FailOpenOnStatusError(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)

	// Create an empty schema with NO thread table, then point a Sidebar at it.
	const emptyDB = "sidebar_failopen_310"
	_, err := ctx.DB().Exec("DROP DATABASE IF EXISTS " + emptyDB)
	require.NoError(t, err)
	_, err = ctx.DB().Exec("CREATE DATABASE " + emptyDB + " CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")
	require.NoError(t, err)
	defer ctx.DB().Exec("DROP DATABASE IF EXISTS " + emptyDB)

	failCfg := config.New()
	failCfg.Test = true
	failCfg.DB.MySQLAddr = "root:demo@tcp(127.0.0.1:3306)/" + emptyDB + "?charset=utf8mb4&parseTime=true"
	failCfg.DB.Migration = false
	failCtx := config.NewContext(failCfg)
	sb := NewSidebar(failCtx)

	cutoffs := recentCutoffs{group: 0, thread: 0, person: 0}
	stubConvs := []*config.SyncUserConversationResp{
		{ChannelID: "g310e____thr", ChannelType: common.ChannelTypeCommunityTopic.Uint8(), Timestamp: 100},
	}
	items := buildRecentItems(stubConvs, cutoffs, nil, nil, nil, "")

	// Mirror the handler's recent branch: query may fail → fail open.
	statusMap, qerr := sb.loadThreadStatuses(items)
	require.Error(t, qerr, "missing thread table must surface a query error")
	if qerr == nil {
		backfillThreadStatus(items, statusMap)
	}

	require.Len(t, items, 1, "fail-open: item list must still be returned")
	assert.Equal(t, 0, items[0].Status,
		"fail-open: thread status must be left unset (omitempty → absent on the wire)")
}
