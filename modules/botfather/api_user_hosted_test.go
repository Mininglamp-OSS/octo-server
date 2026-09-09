package botfather

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func postHostedBot(t *testing.T, route http.Handler, token, name, spaceID string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodPost, "/v1/user/bots", token, map[string]string{
		"name":          name,
		"space_id":      spaceID,
		"agent_hosting": agentHostingOctoHosted,
	}))
	return w
}

func TestCreateHostedBotIsGlobalPerOwnerAndRecoverable(t *testing.T) {
	route, ctx := newUserAPITestServer(t)
	resetUIDRateLimit(t, ctx)

	uid := "hosted_owner_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, uid, "Hosted Owner")
	spaceA := "hosted_a_" + util.GenerUUID()[:8]
	spaceB := "hosted_b_" + util.GenerUUID()[:8]
	addBotToSpace(t, ctx, spaceA, uid)
	addBotToSpace(t, ctx, spaceB, uid)
	token := mintUserAPIKey(t, ctx, uid)

	first := postHostedBot(t, route, token, "First", spaceA)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	var firstBody CreateBotResp
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &firstBody))
	require.NotEmpty(t, firstBody.BotToken)

	second := postHostedBot(t, route, token, "Second", spaceB)
	// /v1/user/bots is a legacy endpoint, so the i18n envelope intentionally
	// remains wire-400 while carrying the semantic 409 in error.http_status.
	require.Equal(t, http.StatusBadRequest, second.Code, second.Body.String())
	var conflict errEnvelope
	require.NoError(t, json.Unmarshal(second.Body.Bytes(), &conflict))
	assert.Equal(t, "err.server.botfather.hosted_bot_exists", conflict.Error.Code)
	assert.Equal(t, http.StatusConflict, conflict.Error.HTTPStatus)
	assert.Empty(t, conflict.Error.Details, "must not expose the existing Bot or Space")

	var active int
	require.NoError(t, ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM robot WHERE creator_uid=? AND status=1 AND agent_hosting=?",
		uid, agentHostingOctoHosted,
	).LoadOne(&active))
	assert.Equal(t, 1, active)
	var allOwned int
	require.NoError(t, ctx.DB().SelectBySql("SELECT COUNT(*) FROM robot WHERE creator_uid=?", uid).LoadOne(&allOwned))
	assert.Equal(t, 1, allOwned, "the rejected attempt must not leave any Bot artifact")

	otherUID := "hosted_other_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, otherUID, "Other Owner")
	otherToken := mintUserAPIKey(t, ctx, otherUID)
	other := postHostedBot(t, route, otherToken, "Other", "")
	require.Equal(t, http.StatusOK, other.Code, other.Body.String())

	// A normal Bot remains allowed while the hosted slot is occupied.
	normal := httptest.NewRecorder()
	route.ServeHTTP(normal, userAPIRequest(t, http.MethodPost, "/v1/user/bots", token, map[string]string{
		"name": "Normal", "space_id": spaceB,
	}))
	require.Equal(t, http.StatusOK, normal.Code, normal.Body.String())

	// Once the hosted Bot becomes inactive it no longer occupies the global slot.
	_, err := ctx.DB().Update("robot").Set("status", 0).Where("robot_id=?", firstBody.RobotID).Exec()
	require.NoError(t, err)
	recreated := postHostedBot(t, route, token, "Recreated", spaceB)
	require.Equal(t, http.StatusOK, recreated.Code, recreated.Body.String())
}

func TestCreateHostedBotConcurrentRequestsHaveOneWinner(t *testing.T) {
	route, ctx := newUserAPITestServer(t)
	resetUIDRateLimit(t, ctx)

	uid := "hosted_race_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, uid, "Race Owner")
	token := mintUserAPIKey(t, ctx, uid)

	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for _, name := range []string{"Race A", "Race B"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			<-start
			responses <- postHostedBot(t, route, token, name, "")
		}(name)
	}
	close(start)
	wg.Wait()
	close(responses)

	var success, rejected int
	for response := range responses {
		switch response.Code {
		case http.StatusOK:
			success++
		case http.StatusBadRequest:
			var conflict errEnvelope
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &conflict))
			if conflict.Error.Code == "err.server.botfather.hosted_bot_exists" {
				rejected++
			}
		default:
			t.Fatalf("unexpected concurrent create status %d: %s", response.Code, response.Body.String())
		}
	}
	assert.Equal(t, 1, success)
	assert.Equal(t, 1, rejected)

	var active int
	require.NoError(t, ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM robot WHERE creator_uid=? AND status=1 AND agent_hosting=?",
		uid, agentHostingOctoHosted,
	).LoadOne(&active))
	assert.Equal(t, 1, active)
}

func TestGetHostedBotReturnsOnlyOwnersCredential(t *testing.T) {
	route, ctx := newUserAPITestServer(t)
	resetUIDRateLimit(t, ctx)

	owner := "hosted_lookup_owner_" + util.GenerUUID()[:8]
	other := "hosted_lookup_other_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, owner, "Hosted Owner")
	insertTestUser(t, ctx, other, "Other Owner")
	ownerKey := mintUserAPIKey(t, ctx, owner)
	otherKey := mintUserAPIKey(t, ctx, other)

	created := postHostedBot(t, route, ownerKey, "Owner Clone", "")
	require.Equal(t, http.StatusOK, created.Code, created.Body.String())
	var createdBody CreateBotResp
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &createdBody))

	lookup := httptest.NewRecorder()
	route.ServeHTTP(lookup, userAPIRequest(t, http.MethodGet, "/v1/user/bots/hosted", ownerKey, nil))
	require.Equal(t, http.StatusOK, lookup.Code, lookup.Body.String())
	var found HostedBotResp
	require.NoError(t, json.Unmarshal(lookup.Body.Bytes(), &found))
	assert.True(t, found.Exists)
	assert.Equal(t, createdBody.RobotID, found.RobotID)
	assert.Equal(t, createdBody.BotToken, found.BotToken)
	assert.Equal(t, "Owner Clone", found.Name)

	notFound := httptest.NewRecorder()
	route.ServeHTTP(notFound, userAPIRequest(t, http.MethodGet, "/v1/user/bots/hosted", otherKey, nil))
	require.Equal(t, http.StatusOK, notFound.Code, notFound.Body.String())
	var absent HostedBotResp
	require.NoError(t, json.Unmarshal(notFound.Body.Bytes(), &absent))
	assert.False(t, absent.Exists)
	assert.Empty(t, absent.BotToken)
}
