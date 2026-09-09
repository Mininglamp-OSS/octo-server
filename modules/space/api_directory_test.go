package space

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/require"
)

type directoryResponseForTest struct {
	Data             []directoryMemberForTest `json:"data"`
	DigitalEmployees []directoryAgentForTest  `json:"digital_employees"`
}

type directoryMemberForTest struct {
	UID             string                  `json:"uid"`
	Name            string                  `json:"name"`
	Role            int                     `json:"role"`
	AgentCount      int64                   `json:"agent_count"`
	AgentsTruncated bool                    `json:"agents_truncated"`
	Agents          []directoryAgentForTest `json:"agents"`
}

type directoryAgentForTest struct {
	UID               string  `json:"uid"`
	Name              string  `json:"name"`
	Description       string  `json:"description"`
	IsFriend          bool    `json:"is_friend"`
	Hosting           string  `json:"hosting"`
	HostingReportedAt *string `json:"hosting_reported_at"`
	BotType           string  `json:"bot_type"`
}

type memberResponseForTest struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
}

func getSpaceDirectory(t *testing.T, srv *server.Server, ctx *config.Context, query url.Values, token string) *httptest.ResponseRecorder {
	t.Helper()
	resetSpaceUIDRateLimit(t, ctx)
	path := "/v1/space/directory"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("token", token)
	}
	srv.GetRoute().ServeHTTP(w, req)
	return w
}

func decodeDirectoryResponse(t *testing.T, w *httptest.ResponseRecorder) directoryResponseForTest {
	t.Helper()
	var resp directoryResponseForTest
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	return resp
}

func getSpaceMembers(t *testing.T, srv *server.Server, spaceID string) []memberResponseForTest {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/space/"+spaceID+"/members?limit=100", nil)
	req.Header.Set("token", testutil.Token)
	srv.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var members []memberResponseForTest
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &members), w.Body.String())
	return members
}

func findMemberResponse(t *testing.T, members []memberResponseForTest, uid string) memberResponseForTest {
	t.Helper()
	for _, member := range members {
		if member.UID == uid {
			return member
		}
	}
	t.Fatalf("member response %q not found", uid)
	return memberResponseForTest{}
}

func findDirectoryMember(t *testing.T, members []directoryMemberForTest, uid string) directoryMemberForTest {
	t.Helper()
	for _, member := range members {
		if member.UID == uid {
			return member
		}
	}
	t.Fatalf("directory member %q not found", uid)
	return directoryMemberForTest{}
}

func findDirectoryAgent(t *testing.T, agents []directoryAgentForTest, uid string) directoryAgentForTest {
	t.Helper()
	for _, agent := range agents {
		if agent.UID == uid {
			return agent
		}
	}
	t.Fatalf("directory agent %q not found", uid)
	return directoryAgentForTest{}
}

func seedDirectoryUser(t *testing.T, uid, name string, robot, status, isDestroy int) {
	t.Helper()
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO `user` (uid, name, robot, status, is_destroy) VALUES (?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE name=VALUES(name), robot=VALUES(robot), status=VALUES(status), is_destroy=VALUES(is_destroy)",
		uid, name, robot, status, isDestroy,
	).Exec()
	require.NoError(t, err)
}

func seedDirectoryMember(t *testing.T, spaceID, uid string, role, status int) {
	t.Helper()
	require.NoError(t, testSpaceDB.insertMemberNoTx(&MemberModel{
		SpaceId: spaceID,
		UID:     uid,
		Role:    role,
		Status:  status,
	}))
}

func seedDirectorySpace(t *testing.T, spaceID string) {
	t.Helper()
	seedDirectoryUser(t, testutil.UID, "Directory Caller", 0, 1, 0)
	require.NoError(t, testSpaceDB.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceID,
		Name:    spaceID,
		Creator: testutil.UID,
		Status:  SpaceStatusNormal,
	}))
	seedDirectoryMember(t, spaceID, testutil.UID, 2, 1)
}

func seedDirectoryBot(t *testing.T, spaceID, ownerUID, botUID, name, description, hosting string, reportedAt *time.Time, status, memberStatus int) {
	t.Helper()
	seedDirectoryUser(t, botUID, name, 1, 1, 0)
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, token, status, creator_uid, description, agent_hosting, agent_reported_hosting_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
		botUID, "token-"+botUID, status, ownerUID, description, hosting, reportedAt,
	).Exec()
	require.NoError(t, err)
	if memberStatus >= 0 {
		seedDirectoryMember(t, spaceID, botUID, 0, memberStatus)
	}
}

func seedDirectoryFriend(t *testing.T, uid, botUID string) {
	t.Helper()
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO friend (uid, to_uid, is_deleted) VALUES (?, ?, 0) ON DUPLICATE KEY UPDATE is_deleted=0",
		uid, botUID,
	).Exec()
	require.NoError(t, err)
}

func TestSpaceDirectoryReturnsHumanOwnersAndCloudAgents(t *testing.T) {
	srv, _, err := setup(t)
	require.NoError(t, err)
	spaceID := fmt.Sprintf("sp-directory-all-%d", time.Now().UnixNano())
	seedDirectorySpace(t, spaceID)

	seedDirectoryUser(t, "owner-real-name", "", 0, 1, 0)
	seedFallbackVerification(t, "owner-real-name", "Real Name")
	seedDirectoryMember(t, spaceID, "owner-real-name", 0, 1)
	seedDirectoryUser(t, "owner-placeholder", "", 0, 1, 0)
	seedDirectoryMember(t, spaceID, "owner-placeholder", 0, 1)
	seedDirectoryUser(t, "owner-empty", "No Agents", 0, 1, 0)
	seedDirectoryMember(t, spaceID, "owner-empty", 0, 1)

	// A system UID can be stored as a human member; it must not become a directory row.
	seedDirectoryUser(t, "fileHelper", "File Helper", 0, 1, 0)
	seedDirectoryMember(t, spaceID, "fileHelper", 0, 1)

	reportedAt := time.Date(2026, time.September, 3, 10, 0, 0, 0, time.UTC)
	withdrawnAt := time.Date(2026, time.September, 4, 11, 0, 0, 0, time.UTC)
	seedDirectoryBot(t, spaceID, testutil.UID, "bot-unreported", "Unreported", "no report", "", nil, 1, 1)
	seedDirectoryBot(t, spaceID, testutil.UID, "bot-withdrawn", "Withdrawn", "was reported", "", &withdrawnAt, 1, 1)
	seedDirectoryBot(t, spaceID, "owner-real-name", "bot-octo", "Octo Bot", "cloud", "octo_hosted", &reportedAt, 1, 1)
	seedDirectoryBot(t, spaceID, "owner-real-name", "bot-octo-null-ts", "Octo Null TS", "cloud no ts", "octo_hosted", nil, 1, 1)
	seedDirectoryBot(t, spaceID, "owner-real-name", "bot-vendor", "Vendor Bot", "third party", "vendor_hosted", &reportedAt, 1, 1)
	seedDirectoryFriend(t, testutil.UID, "bot-octo")
	avatarID := "avatar-directory"
	seedDirectoryUser(t, avatarID, "Digital Employee", 1, 1, 0)
	_, err = testCtx.DB().InsertBySql(`INSERT INTO robot
		(robot_id,token,status,creator_uid,description,auto_approve,kind,management_scope,
		 management_space_id,created_by,publication_state,lifecycle_pending)
		VALUES (?, ?,1,'','organization managed',1,'avatar','space',?,'manager','published',0)`,
		avatarID, "token-"+avatarID, spaceID).Exec()
	require.NoError(t, err)
	seedDirectoryMember(t, spaceID, avatarID, 0, 1)
	seedDirectoryFriend(t, testutil.UID, avatarID)

	seedDirectoryBot(t, spaceID, "owner-real-name", "bot-local", "Local", "excluded", "self_hosted", &reportedAt, 1, 1)
	seedDirectoryBot(t, spaceID, "owner-real-name", "bot-inactive", "Inactive", "excluded", "octo_hosted", &reportedAt, 0, 1)
	seedDirectoryBot(t, spaceID, "owner-real-name", "bot-removed", "Removed", "excluded", "octo_hosted", &reportedAt, 1, 0)
	seedDirectoryBot(t, spaceID, "owner-real-name", "bot-outside", "Outside", "excluded", "octo_hosted", &reportedAt, 1, -1)
	seedDirectoryBot(t, spaceID, "owner-missing", "bot-orphan", "Orphan", "excluded", "octo_hosted", &reportedAt, 1, 1)

	// Creator is itself a bot: octo_hosted but not owned by a human.
	seedDirectoryUser(t, "bot-as-owner", "Bot As Owner", 1, 1, 0)
	seedDirectoryMember(t, spaceID, "bot-as-owner", 0, 1)
	seedDirectoryBot(t, spaceID, "bot-as-owner", "bot-nested", "Nested", "excluded", "octo_hosted", &reportedAt, 1, 1)

	seedDirectoryUser(t, "owner-inactive", "Inactive Owner", 0, 0, 0)
	seedDirectoryMember(t, spaceID, "owner-inactive", 0, 1)
	seedDirectoryBot(t, spaceID, "owner-inactive", "bot-inactive-owner", "Inactive Owner Bot", "excluded", "octo_hosted", &reportedAt, 1, 1)
	seedDirectoryUser(t, "owner-destroyed", "Destroyed Owner", 0, 1, 2)
	seedDirectoryMember(t, spaceID, "owner-destroyed", 0, 1)
	seedDirectoryBot(t, spaceID, "owner-destroyed", "bot-destroyed-owner", "Destroyed Owner Bot", "excluded", "octo_hosted", &reportedAt, 1, 1)
	seedDirectoryBot(t, spaceID, "", "bot-empty-owner", "Empty Owner Bot", "excluded", "octo_hosted", &reportedAt, 1, 1)

	w := getSpaceDirectory(t, srv, testCtx, url.Values{"space_id": {spaceID}}, testutil.Token)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"hosting_reported_at":null`, "unreported hosting time must be an explicit null, not an omitted key")
	resp := decodeDirectoryResponse(t, w)
	require.Len(t, resp.Data, 4, "only active non-system humans are directory rows")

	caller := findDirectoryMember(t, resp.Data, testutil.UID)
	require.Equal(t, int64(0), caller.AgentCount, "empty and retracted hosting must not count as cloud agents")
	require.False(t, caller.AgentsTruncated)
	require.Empty(t, caller.Agents)

	owner := findDirectoryMember(t, resp.Data, "owner-real-name")
	require.Equal(t, "Real Name", owner.Name, "directory name must follow MemberDetailModel.DisplayName")
	require.Equal(t, findMemberResponse(t, getSpaceMembers(t, srv, spaceID), "owner-real-name").Name, owner.Name,
		"directory and members endpoints must share the display-name fallback chain")
	require.Equal(t, int64(2), owner.AgentCount)
	require.Len(t, owner.Agents, 2)
	octo := findDirectoryAgent(t, owner.Agents, "bot-octo")
	require.True(t, octo.IsFriend)
	require.Equal(t, "octo_hosted", octo.Hosting)
	require.Equal(t, "cloud", octo.Description)
	require.NotNil(t, octo.HostingReportedAt)
	require.Equal(t, "2026-09-03 10:00:00", *octo.HostingReportedAt)
	nullTS := findDirectoryAgent(t, owner.Agents, "bot-octo-null-ts")
	require.False(t, nullTS.IsFriend)
	require.Equal(t, "octo_hosted", nullTS.Hosting)
	require.Nil(t, nullTS.HostingReportedAt)
	require.Equal(t, "user_bot", findDirectoryAgent(t, owner.Agents, "bot-octo").BotType)
	require.Len(t, resp.DigitalEmployees, 1)
	digitalEmployee := findDirectoryAgent(t, resp.DigitalEmployees, avatarID)
	require.Equal(t, "avatar", digitalEmployee.BotType)
	require.Equal(t, "Digital Employee", digitalEmployee.Name)
	require.True(t, digitalEmployee.IsFriend)

	placeholder := findDirectoryMember(t, resp.Data, "owner-placeholder")
	require.Equal(t, memberDisplayNamePlaceholderPrefix+"owner-placeholder", placeholder.Name)
	require.Equal(t, findMemberResponse(t, getSpaceMembers(t, srv, spaceID), "owner-placeholder").Name, placeholder.Name,
		"directory and members endpoints must share the stable placeholder fallback")

	empty := findDirectoryMember(t, resp.Data, "owner-empty")
	require.Equal(t, int64(0), empty.AgentCount)
	require.False(t, empty.AgentsTruncated)
	require.NotNil(t, empty.Agents, "empty agent lists must encode as [] rather than null")
	require.Empty(t, empty.Agents)

	for _, member := range resp.Data {
		require.NotEqual(t, "fileHelper", member.UID)
		require.NotEqual(t, "bot-as-owner", member.UID, "a bot must not appear as a directory owner row")
		for _, agent := range member.Agents {
			require.NotContains(t, []string{
				"bot-unreported", "bot-withdrawn", "bot-vendor", "bot-local",
				"bot-inactive", "bot-removed", "bot-outside", "bot-orphan",
				"bot-nested", "bot-as-owner",
				"bot-inactive-owner", "bot-destroyed-owner", "bot-empty-owner",
			}, agent.UID)
			require.Equal(t, "octo_hosted", agent.Hosting)
		}
	}

	explicitFalse := getSpaceDirectory(t, srv, testCtx, url.Values{
		"space_id":         {spaceID},
		"only_with_agents": {"false"},
	}, testutil.Token)
	require.Equal(t, http.StatusOK, explicitFalse.Code, explicitFalse.Body.String())
	require.Len(t, decodeDirectoryResponse(t, explicitFalse).Data, 4)

	onlyWithAgents := getSpaceDirectory(t, srv, testCtx, url.Values{
		"space_id":         {spaceID},
		"only_with_agents": {"true"},
	}, testutil.Token)
	require.Equal(t, http.StatusOK, onlyWithAgents.Code, onlyWithAgents.Body.String())
	filtered := decodeDirectoryResponse(t, onlyWithAgents)
	require.Len(t, filtered.Data, 1, "only humans who own octo_hosted bots remain")
	require.Equal(t, "owner-real-name", filtered.Data[0].UID)
	require.Positive(t, filtered.Data[0].AgentCount)
}

func TestSpaceDirectoryKeywordFiltersHumanAndVisibleBotNames(t *testing.T) {
	srv, _, err := setup(t)
	require.NoError(t, err)
	spaceID := fmt.Sprintf("sp-directory-keyword-%d", time.Now().UnixNano())
	seedDirectorySpace(t, spaceID)

	seedDirectoryUser(t, "owner-human-match", "Alice Filter", 0, 1, 0)
	seedDirectoryMember(t, spaceID, "owner-human-match", 0, 1)
	seedDirectoryBot(t, spaceID, "owner-human-match", "bot-human-other", "Ordinary Cloud Bot", "", "octo_hosted", nil, 1, 1)

	seedDirectoryUser(t, "owner-bot-match", "Bob", 0, 1, 0)
	seedDirectoryMember(t, spaceID, "owner-bot-match", 0, 1)
	seedDirectoryBot(t, spaceID, "owner-bot-match", "bot-keyword", "Needle Bot", "", "octo_hosted", nil, 1, 1)

	seedDirectoryUser(t, "owner-local-only", "Carol", 0, 1, 0)
	seedDirectoryMember(t, spaceID, "owner-local-only", 0, 1)
	seedDirectoryBot(t, spaceID, "owner-local-only", "bot-local-keyword", "Needle Local", "", "self_hosted", nil, 1, 1)

	seedDirectoryUser(t, "owner-vendor-only", "Dave", 0, 1, 0)
	seedDirectoryMember(t, spaceID, "owner-vendor-only", 0, 1)
	seedDirectoryBot(t, spaceID, "owner-vendor-only", "bot-vendor-keyword", "Needle Vendor", "", "vendor_hosted", nil, 1, 1)

	seedDirectoryUser(t, "owner-empty-only", "Eve", 0, 1, 0)
	seedDirectoryMember(t, spaceID, "owner-empty-only", 0, 1)
	seedDirectoryBot(t, spaceID, "owner-empty-only", "bot-empty-keyword", "Needle Empty", "", "", nil, 1, 1)

	seedDirectoryUser(t, "bot-as-owner-kw", "Bot Owner", 1, 1, 0)
	seedDirectoryMember(t, spaceID, "bot-as-owner-kw", 0, 1)
	seedDirectoryBot(t, spaceID, "bot-as-owner-kw", "bot-nested-keyword", "Needle Nested", "", "octo_hosted", nil, 1, 1)

	seedDirectoryUser(t, "owner-literal", "Literal Owner", 0, 1, 0)
	seedDirectoryMember(t, spaceID, "owner-literal", 0, 1)
	seedDirectoryBot(t, spaceID, "owner-literal", "bot-literal", "A_100% Bot", "", "octo_hosted", nil, 1, 1)
	seedDirectoryUser(t, "owner-wildcard-near", "Near Owner", 0, 1, 0)
	seedDirectoryMember(t, spaceID, "owner-wildcard-near", 0, 1)
	seedDirectoryBot(t, spaceID, "owner-wildcard-near", "bot-wildcard-near", "Ax100suffix", "", "octo_hosted", nil, 1, 1)

	humanName := getSpaceDirectory(t, srv, testCtx, url.Values{
		"space_id": {spaceID},
		"keyword":  {"Alice"},
	}, testutil.Token)
	require.Equal(t, http.StatusOK, humanName.Code, humanName.Body.String())
	humanResp := decodeDirectoryResponse(t, humanName)
	require.Len(t, humanResp.Data, 1)
	human := findDirectoryMember(t, humanResp.Data, "owner-human-match")
	require.Zero(t, human.AgentCount, "only Bot-name matches are returned as agents for a keyword search")
	require.Empty(t, human.Agents)

	humanNameWithAgentsOnly := getSpaceDirectory(t, srv, testCtx, url.Values{
		"space_id":         {spaceID},
		"keyword":          {"Alice"},
		"only_with_agents": {"true"},
	}, testutil.Token)
	require.Equal(t, http.StatusOK, humanNameWithAgentsOnly.Code, humanNameWithAgentsOnly.Body.String())
	require.Empty(t, decodeDirectoryResponse(t, humanNameWithAgentsOnly).Data)

	botName := getSpaceDirectory(t, srv, testCtx, url.Values{
		"space_id": {spaceID},
		"keyword":  {"Needle Bot"},
	}, testutil.Token)
	require.Equal(t, http.StatusOK, botName.Code, botName.Body.String())
	botResp := decodeDirectoryResponse(t, botName)
	require.Len(t, botResp.Data, 1)
	botOwner := findDirectoryMember(t, botResp.Data, "owner-bot-match")
	require.Equal(t, int64(1), botOwner.AgentCount)
	require.Len(t, botOwner.Agents, 1)
	require.Equal(t, "bot-keyword", botOwner.Agents[0].UID)

	localBotName := getSpaceDirectory(t, srv, testCtx, url.Values{
		"space_id": {spaceID},
		"keyword":  {"Needle Local"},
	}, testutil.Token)
	require.Equal(t, http.StatusOK, localBotName.Code, localBotName.Body.String())
	require.Empty(t, decodeDirectoryResponse(t, localBotName).Data, "self-hosted Bot names must not match")

	vendorBotName := getSpaceDirectory(t, srv, testCtx, url.Values{
		"space_id": {spaceID},
		"keyword":  {"Needle Vendor"},
	}, testutil.Token)
	require.Equal(t, http.StatusOK, vendorBotName.Code, vendorBotName.Body.String())
	require.Empty(t, decodeDirectoryResponse(t, vendorBotName).Data, "vendor-hosted Bot names must not match")

	emptyBotName := getSpaceDirectory(t, srv, testCtx, url.Values{
		"space_id": {spaceID},
		"keyword":  {"Needle Empty"},
	}, testutil.Token)
	require.Equal(t, http.StatusOK, emptyBotName.Code, emptyBotName.Body.String())
	require.Empty(t, decodeDirectoryResponse(t, emptyBotName).Data, "unreported Bot names must not match")

	nestedBotName := getSpaceDirectory(t, srv, testCtx, url.Values{
		"space_id": {spaceID},
		"keyword":  {"Needle Nested"},
	}, testutil.Token)
	require.Equal(t, http.StatusOK, nestedBotName.Code, nestedBotName.Body.String())
	require.Empty(t, decodeDirectoryResponse(t, nestedBotName).Data, "bots owned by another bot must not match")

	literalName := getSpaceDirectory(t, srv, testCtx, url.Values{
		"space_id": {spaceID},
		"keyword":  {"A_100%"},
	}, testutil.Token)
	require.Equal(t, http.StatusOK, literalName.Code, literalName.Body.String())
	literalResp := decodeDirectoryResponse(t, literalName)
	require.Len(t, literalResp.Data, 1, "LIKE wildcard characters in keyword must be literal")
	literalOwner := findDirectoryMember(t, literalResp.Data, "owner-literal")
	require.Equal(t, int64(1), literalOwner.AgentCount)
	require.Equal(t, "bot-literal", literalOwner.Agents[0].UID)
}

func TestDirectoryQueriesHonorCanceledContext(t *testing.T) {
	_, _, err := setup(t)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = testSpaceDB.queryDirectoryOwners(ctx, "sp-canceled", "")
	require.Error(t, err)
	_, err = testSpaceDB.queryDirectoryAgents(ctx, "sp-canceled", testutil.UID, "")
	require.Error(t, err)
}

func TestSpaceDirectoryCanceledRequestReturnsNoPartialData(t *testing.T) {
	srv, _, err := setup(t)
	require.NoError(t, err)
	spaceID := fmt.Sprintf("sp-dir-canceled-%d", time.Now().UnixNano())
	seedDirectorySpace(t, spaceID)

	requestCtx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/v1/space/directory?space_id="+spaceID, nil).WithContext(requestCtx)
	req.Header.Set("token", testutil.Token)
	cancel()
	resetSpaceUIDRateLimit(t, testCtx)
	w := httptest.NewRecorder()
	srv.GetRoute().ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assertSpaceErrorCode(t, w, "err.server.space.query_failed")
	var envelope map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope), w.Body.String())
	_, hasData := envelope["data"]
	require.False(t, hasData, "a query failure must not encode a partial success envelope")
}

func TestQueryDirectoryAgentsCapsEachOwnerAndKeepsTrueCounts(t *testing.T) {
	_, _, err := setup(t)
	require.NoError(t, err)
	const spaceID = "sp-directory-cap"
	seedDirectorySpace(t, spaceID)
	for _, ownerUID := range []string{testutil.UID, "owner-cap"} {
		if ownerUID != testutil.UID {
			seedDirectoryUser(t, ownerUID, ownerUID, 0, 1, 0)
			seedDirectoryMember(t, spaceID, ownerUID, 0, 1)
		}
		for i := 0; i < 51; i++ {
			botUID := fmt.Sprintf("%s-bot-%03d", ownerUID, i)
			seedDirectoryBot(t, spaceID, ownerUID, botUID, botUID, "", "octo_hosted", nil, 1, 1)
		}
	}

	first, err := testSpaceDB.queryDirectoryAgents(context.Background(), spaceID, testutil.UID, "")
	require.NoError(t, err)
	second, err := testSpaceDB.queryDirectoryAgents(context.Background(), spaceID, testutil.UID, "")
	require.NoError(t, err)
	require.Len(t, first, 100, "the SQL query must enforce the per-owner cap before data reaches Go")
	require.Len(t, second, 100)

	byOwner := make(map[string][]*directoryAgentModel)
	for _, agent := range first {
		byOwner[agent.CreatorUID] = append(byOwner[agent.CreatorUID], agent)
		require.Equal(t, int64(51), agent.AgentCount)
	}
	require.Len(t, byOwner[testutil.UID], 50)
	require.Len(t, byOwner["owner-cap"], 50)

	for i := range first {
		require.Equal(t, first[i].CreatorUID, second[i].CreatorUID)
		require.Equal(t, first[i].UID, second[i].UID, "robot_id ordering must make the selected capped set stable")
	}
}

func TestSpaceDirectoryExactPerOwnerCapIsNotTruncated(t *testing.T) {
	srv, _, err := setup(t)
	require.NoError(t, err)
	const spaceID = "sp-directory-exact-cap"
	seedDirectorySpace(t, spaceID)
	for i := 0; i < directoryAgentsPerOwner; i++ {
		botUID := fmt.Sprintf("exact-cap-bot-%03d", i)
		seedDirectoryBot(t, spaceID, testutil.UID, botUID, botUID, "", "octo_hosted", nil, 1, 1)
	}

	w := getSpaceDirectory(t, srv, testCtx, url.Values{"space_id": {spaceID}}, testutil.Token)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	member := findDirectoryMember(t, decodeDirectoryResponse(t, w).Data, testutil.UID)
	require.Equal(t, int64(directoryAgentsPerOwner), member.AgentCount)
	require.Len(t, member.Agents, directoryAgentsPerOwner)
	require.False(t, member.AgentsTruncated, "exactly 50 agents is not a truncated result")
}

func TestSpaceDirectoryOverPerOwnerCapIsTruncated(t *testing.T) {
	srv, _, err := setup(t)
	require.NoError(t, err)
	const spaceID = "sp-directory-over-cap"
	seedDirectorySpace(t, spaceID)
	for i := 0; i <= directoryAgentsPerOwner; i++ {
		botUID := fmt.Sprintf("over-cap-bot-%03d", i)
		seedDirectoryBot(t, spaceID, testutil.UID, botUID, botUID, "", "octo_hosted", nil, 1, 1)
	}

	w := getSpaceDirectory(t, srv, testCtx, url.Values{"space_id": {spaceID}}, testutil.Token)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	member := findDirectoryMember(t, decodeDirectoryResponse(t, w).Data, testutil.UID)
	require.Equal(t, int64(directoryAgentsPerOwner+1), member.AgentCount)
	require.Len(t, member.Agents, directoryAgentsPerOwner)
	require.True(t, member.AgentsTruncated, "more than 50 agents is a truncated result")
}

func TestSpaceDirectoryAuthzAndRequestValidation(t *testing.T) {
	srv, _, err := setup(t)
	require.NoError(t, err)

	missing := getSpaceDirectory(t, srv, testCtx, nil, testutil.Token)
	require.Equal(t, http.StatusBadRequest, missing.Code, missing.Body.String())
	assertSpaceErrorCode(t, missing, "err.server.space.request_invalid")

	foreignSpace := fmt.Sprintf("sp-directory-foreign-%d", time.Now().UnixNano())
	seedDirectoryUser(t, "foreign-owner", "Foreign Owner", 0, 1, 0)
	require.NoError(t, testSpaceDB.insertSpaceNoTx(&SpaceModel{
		SpaceId: foreignSpace,
		Name:    foreignSpace,
		Creator: "foreign-owner",
		Status:  SpaceStatusNormal,
	}))
	seedDirectoryMember(t, foreignSpace, "foreign-owner", 2, 1)
	nonMember := getSpaceDirectory(t, srv, testCtx, url.Values{"space_id": {foreignSpace}}, testutil.Token)
	require.Equal(t, http.StatusForbidden, nonMember.Code, nonMember.Body.String())

	unauthenticated := getSpaceDirectory(t, srv, testCtx, url.Values{"space_id": {foreignSpace}}, "")
	require.Equal(t, http.StatusUnauthorized, unauthenticated.Code, unauthenticated.Body.String())
}
