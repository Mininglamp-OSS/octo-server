package ai_team_test

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReconcileReAddingReadyAgentDoesNotReprovisionOnIMFailure(t *testing.T) {
	f := seedFixture(t)
	w := request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	w = request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID+"/sessions", "ready-before-reconcile", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var calls atomic.Int32
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/channel/subscriber_reconcile" {
			calls.Add(1)
		}
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer failing.Close()
	cfg := testContext.GetConfig()
	previous := cfg.WuKongIM.APIURL
	cfg.WuKongIM.APIURL = failing.URL
	defer func() { cfg.WuKongIM.APIURL = previous }()
	t.Setenv("DM_IM_RECONCILE_ENABLED", "true")
	w = request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var state int
	require.NoError(t, testContext.DB().Select("container_state").From("ai_team_agent").Where("space_id=? AND user_uid=? AND bot_id=?", f.spaceID, f.uid, f.botID).LoadOne(&state))
	require.Equal(t, 2, state)
	require.Zero(t, calls.Load(), "healthy no-op must not depend on IM availability")
	var intents int
	require.NoError(t, testContext.DB().QueryRow("SELECT COUNT(*) FROM im_group_reconcile").Scan(&intents))
	require.Zero(t, intents)
}
