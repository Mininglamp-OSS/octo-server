package user

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/require"
)

type botContextUserNames struct{ IService }

func (botContextUserNames) GetUser(uid string) (*Resp, error) {
	return &Resp{Name: uid + " name"}, nil
}

// Real HTTP handler and SQL contract; this does not replace MySQL integration.
// Opt-in fields must never change default verifier clients.
func TestBotOwnerContextHTTPContract(t *testing.T) {
	for _, tc := range []struct {
		name, query, body          string
		invalid, extended, dbError bool
	}{
		{"default", "", `{"bot_token":"bf_test"}`, false, false, false},
		{"legacy context", "?include=context", `{"bot_token":"bf_test","owner_uid":"ignored"}`, false, false, false},
		{"scoped", "?include=owner_context", `{"bot_token":"bf_test","space_id":"space","project_ids":["p"]}`, false, true, false},
		{"unavailable", "?include=owner_context", `{"bot_token":"bf_test","space_id":"space","project_ids":["p"]}`, false, true, true},
		{"missing space", "?include=owner_context", `{"bot_token":"bf_test"}`, true, false, false},
		{"forged owner", "?include=owner_context", `{"bot_token":"bf_test","space_id":"space","owner_uid":"victim"}`, true, false, false},
		{"too many projects", "?include=owner_context", `{"bot_token":"bf_test","space_id":"space","project_ids":[` + strings.Repeat(`"p",`, maxVerifyProjectIDs) + `"p"]}`, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock := newPhoneLookupMock(t)
			u := &User{db: db, userService: botContextUserNames{}, Log: log.NewTLog("bot-context-test")}
			if !tc.invalid {
				mock.ExpectQuery("SELECT.*FROM robot.*bot_token = 'bf_test'").WillReturnRows(sqlmock.NewRows([]string{"robot_id", "creator_uid"}).AddRow("bot", "owner"))
				mock.ExpectQuery("SELECT.*FROM space_member.*uid = 'bot'").WillReturnRows(sqlmock.NewRows([]string{"space_id"}).AddRow("first-space"))
				if tc.extended {
					q := mock.ExpectQuery("SELECT IFNULL.*WHERE r.robot_id = 'bot' AND r.creator_uid = 'owner' AND r.bot_token = 'bf_test'")
					if tc.dbError {
						q.WillReturnError(errors.New("database unavailable"))
					} else {
						q.WillReturnRows(sqlmock.NewRows([]string{"hosting", "bot_active", "owner_active", "bot_member", "owner_member"}).AddRow("self_hosted", true, true, true, true))
						mock.ExpectQuery("SELECT pm.project_id.*WHERE pm.uid = 'owner'.*pm.space_id = 'space'").WillReturnRows(sqlmock.NewRows([]string{"project_id", "role", "member_epoch"}).AddRow("p", 2, 7))
					}
				}
			}
			r := wkhttp.New()
			r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
			r.POST("/v1/auth/verify-bot", u.authVerifyBot)
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/auth/verify-bot"+tc.query, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)
			var response map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
			if tc.invalid {
				require.Equal(t, http.StatusBadRequest, w.Code)
				require.Contains(t, response, "error")
			} else {
				require.Equal(t, http.StatusOK, w.Code)
				require.Equal(t, "first-space", response["space_id"])
				require.NotContains(t, response, "projects")
				require.NotContains(t, w.Body.String(), "bf_test")
				if !tc.extended {
					require.Len(t, response, 5)
				} else if tc.dbError {
					require.Equal(t, true, response["context_error"])
					require.NotContains(t, response, "bot_context")
					require.NotContains(t, response, "owner_context")
				} else {
					require.Equal(t, true, response["context_included"])
					require.Contains(t, response, "bot_context")
					require.Contains(t, response, "owner_context")
					require.NotContains(t, response["bot_context"], "agent_platform")
					require.Equal(t, "self_hosted", response["bot_context"].(map[string]any)["agent_hosting"])
				}
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestBotOwnerContextDefaultSchema(t *testing.T) {
	data, err := json.Marshal(authVerifyBotResp{BotUID: "bot", BotName: "Bot", OwnerUID: "owner", OwnerName: "Owner", SpaceID: "legacy-first-space"})
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(data, &fields))
	require.Len(t, fields, 5)
	for _, key := range []string{"bot_uid", "bot_name", "owner_uid", "owner_name", "space_id"} {
		require.Contains(t, fields, key)
	}
	data, err = json.Marshal(ownedBot{UID: "bot", Name: "Bot"})
	require.NoError(t, err)
	require.JSONEq(t, `{"uid":"bot","name":"Bot"}`, string(data))
}

func TestBotOwnerContextPlatformNeutralAndSpaceScoped(t *testing.T) {
	for _, tc := range []struct {
		name, hosting                                  string
		botActive, ownerActive, botMember, ownerMember bool
	}{
		{"assistant without platform", "self_hosted", true, true, true, true},
		{"cloud facts are not filtered by Server", "octo_hosted", true, true, true, true},
		{"unknown hosting remains an identity fact", "", true, true, true, true},
		{"bot disabled", "self_hosted", false, true, true, true},
		{"owner disabled", "self_hosted", true, false, true, true},
		{"bot removed from Space", "self_hosted", true, true, false, true},
		{"owner removed from Space", "self_hosted", true, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock := newPhoneLookupMock(t)
			mock.ExpectQuery("SELECT IFNULL.*WHERE r.robot_id = 'bot' AND r.creator_uid = 'owner' AND r.bot_token = 'bf_test' AND r.status = 1").
				WillReturnRows(sqlmock.NewRows([]string{"hosting", "bot_active", "owner_active", "bot_member", "owner_member"}).AddRow(tc.hosting, tc.botActive, tc.ownerActive, tc.botMember, tc.ownerMember))
			allowed := tc.botActive && tc.ownerActive && tc.botMember && tc.ownerMember
			if allowed {
				mock.ExpectQuery("SELECT pm.project_id.*WHERE pm.uid = 'owner'.*pm.space_id = 'space'").
					WillReturnRows(sqlmock.NewRows([]string{"project_id", "role", "member_epoch"}).AddRow("p", 2, 7))
			}
			resp := authVerifyBotResp{BotUID: "bot", OwnerUID: "owner", SpaceID: "legacy-first-space"}
			err := (&User{db: db}).fillBotProjectContext(&resp, authVerifyBotReq{BotToken: "bf_test", SpaceID: "space", ProjectIDs: []string{"p", "unknown", "p"}})
			require.NoError(t, err)
			require.Equal(t, "legacy-first-space", resp.SpaceID)
			require.True(t, resp.ContextIncluded)
			require.Len(t, resp.OwnerContext.Projects, 2)
			require.Equal(t, allowed, resp.OwnerContext.Projects[0].Member)
			require.False(t, resp.OwnerContext.Projects[1].Member)
			require.Nil(t, resp.OwnerContext.Projects[1].Role)
			require.Nil(t, resp.OwnerContext.Projects[1].MemberEpoch)
			if !allowed {
				require.Nil(t, resp.OwnerContext.Projects[0].Role)
				require.Nil(t, resp.OwnerContext.Projects[0].MemberEpoch)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestBotOwnerContextFailsClosed(t *testing.T) {
	db, mock := newPhoneLookupMock(t)
	mock.ExpectQuery("SELECT IFNULL").WillReturnError(errors.New("database unavailable"))
	resp := authVerifyBotResp{BotUID: "bot", OwnerUID: "owner", SpaceID: "legacy"}
	err := (&User{db: db}).fillBotProjectContext(&resp, authVerifyBotReq{BotToken: "bf_test", SpaceID: "space", ProjectIDs: []string{"p"}})
	require.Error(t, err)
	require.Empty(t, resp.OwnerContext.Projects)
	require.False(t, resp.BotContext.SpaceMember)
	require.Equal(t, "legacy", resp.SpaceID)
	require.NoError(t, mock.ExpectationsWereMet())
	err = (&User{}).fillBotProjectContext(&resp, authVerifyBotReq{ProjectIDs: make([]string, maxVerifyProjectIDs+1)})
	require.ErrorIs(t, err, errTooManyProjectIDs)
}
