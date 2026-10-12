package group

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/require"
)

func TestReconcileNoOpAdmissionAndRemovalPreserveActiveProgress(t *testing.T) {
	_, ctx := newTestServer(t)
	defer testutil.CleanAllTables(ctx)
	f := New(ctx)
	seedGroupInSpace(t, ctx, "reconcile-noop", "space-noop", "owner")
	t.Setenv("DM_IM_RECONCILE_ENABLED", "true")
	require.NoError(t, admitFull(t, f, "reconcile-noop", MemberAdmission{UID: "member", Version: 1, Role: MemberRoleManager}))
	_, err := ctx.DB().Exec(`UPDATE im_group_reconcile SET active_revision=revision,child_cursor=17,member_page=2,
		attempts=3,lease_owner='working',lease_until=DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 30 SECOND),
		next_attempt_at=DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 30 SECOND),last_error='retry' WHERE group_no='reconcile-noop'`)
	require.NoError(t, err)
	for i := 0; i < 10; i++ {
		require.NoError(t, admitFull(t, f, "reconcile-noop", MemberAdmission{UID: "member", Version: int64(i + 2), Role: MemberRoleCommon}))
	}
	var revision, active uint64
	var child, page, attempts int
	var owner, message string
	require.NoError(t, ctx.DB().QueryRow(`SELECT revision,active_revision,child_cursor,member_page,attempts,lease_owner,last_error
		FROM im_group_reconcile WHERE group_no='reconcile-noop'`).Scan(&revision, &active, &child, &page, &attempts, &owner, &message))
	require.EqualValues(t, 1, revision)
	require.EqualValues(t, 1, active)
	require.Equal(t, 17, child)
	require.Equal(t, 2, page)
	require.Equal(t, 3, attempts)
	require.Equal(t, "working", owner)
	require.Equal(t, "retry", message)
	require.Equal(t, MemberRoleManager, readMemberRow(t, ctx, "reconcile-noop", "member").Role)
	for i := 0; i < 3; i++ {
		tx, err := ctx.DB().Begin()
		require.NoError(t, err)
		require.NoError(t, f.db.DeleteMemberTx("reconcile-noop", "member", int64(100+i), tx))
		require.NoError(t, tx.Commit())
	}
	require.NoError(t, ctx.DB().QueryRow("SELECT revision,active_revision FROM im_group_reconcile WHERE group_no='reconcile-noop'").Scan(&revision, &active))
	require.EqualValues(t, 2, revision, "only the actual removal advances authority")
	require.EqualValues(t, 1, active)
	require.EqualValues(t, 100, readMemberRow(t, ctx, "reconcile-noop", "member").Version)
}

func TestReconcileDirectoryRetryDoesNotSendNoticesBeforeAllGroupsSync(t *testing.T) {
	_, ctx := newTestServer(t)
	defer testutil.CleanAllTables(ctx)
	f := New(ctx)
	ensureThreadTables(t, f)
	for _, group := range []string{"org-reconcile-a", "org-reconcile-b"} {
		seedGroupInSpace(t, ctx, group, "org-space", "owner")
	}
	var fail atomic.Bool
	fail.Store(true)
	var notices atomic.Int32
	im := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/channel/subscriber_reconcile" {
			var snapshot struct {
				Channel string `json:"channel_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&snapshot)
			if snapshot.Channel == "org-reconcile-b" && fail.Load() {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		if r.URL.Path == "/message/send" {
			notices.Add(1)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer im.Close()
	cfg := ctx.GetConfig()
	previous := cfg.WuKongIM.APIURL
	cfg.WuKongIM.APIURL = im.URL
	defer func() { cfg.WuKongIM.APIURL = previous }()
	t.Setenv("DM_IM_RECONCILE_ENABLED", "true")
	body, err := json.Marshal(config.MsgOrgOrDeptEmployeeUpdateReq{Members: []*config.OrgOrDeptEmployeeVO{
		{GroupNo: "org-reconcile-a", EmployeeUid: "org-member", EmployeeName: "Member", Operator: "owner", Action: "add"},
		{GroupNo: "org-reconcile-b", EmployeeUid: "org-member", EmployeeName: "Member", Operator: "owner", Action: "add"},
	}})
	require.NoError(t, err)
	var commitErr error
	f.handleOrgOrDeptEmployeeUpdate(body, func(err error) { commitErr = err })
	require.Error(t, commitErr)
	require.Zero(t, notices.Load())
	fail.Store(false)
	_, err = ctx.DB().Exec("UPDATE im_group_reconcile SET next_attempt_at=UTC_TIMESTAMP(6)")
	require.NoError(t, err)
	f.handleOrgOrDeptEmployeeUpdate(body, func(err error) { commitErr = err })
	require.NoError(t, commitErr)
	require.EqualValues(t, 2, notices.Load())
	var maxRevision uint64
	require.NoError(t, ctx.DB().QueryRow("SELECT MAX(revision) FROM im_group_reconcile").Scan(&maxRevision))
	require.EqualValues(t, 1, maxRevision)
}

func TestReconcileExitDistinguishesNeverMemberFromPreviousExit(t *testing.T) {
	_, ctx := newTestServer(t)
	defer testutil.CleanAllTables(ctx)
	ctx.Event = noopGroupEvent{}
	f := New(ctx)
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	f.Route(r)
	seedSpaceSeat(t, ctx, "exit-space", testutil.UID)
	seedGroupInSpace(t, ctx, "reconcile-exit", "exit-space", "other-owner")
	t.Setenv("DM_IM_RECONCILE_ENABLED", "true")
	_, err := ctx.DB().Exec(`INSERT INTO im_group_reconcile(group_no,revision,completed_revision,pending,next_attempt_at)
		VALUES ('reconcile-exit',1,1,0,UTC_TIMESTAMP(6))`)
	require.NoError(t, err)
	request := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/groups/reconcile-exit/exit", nil)
		req.Header.Set("token", testutil.Token)
		req.Header.Set("X-Space-ID", "exit-space")
		r.ServeHTTP(w, req)
		return w
	}
	w := request()
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "err.server.group.member_not_in_group")
	seedGroupMember(t, ctx, "reconcile-exit", testutil.UID, MemberRoleCommon)
	_, err = ctx.DB().Exec("UPDATE group_member SET is_deleted=1 WHERE group_no='reconcile-exit' AND uid=?", testutil.UID)
	require.NoError(t, err)
	w = request()
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}
