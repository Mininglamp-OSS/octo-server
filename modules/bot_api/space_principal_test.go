package bot_api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type fakeSpacePrincipalStore struct {
	principal *spacePrincipal
	err       error
	calls     [][4]string
}

func (f *fakeSpacePrincipalStore) lookupEligibleSpacePrincipal(callerUID, callerKind, spaceID, targetUID string) (*spacePrincipal, error) {
	f.calls = append(f.calls, [4]string{callerUID, callerKind, spaceID, targetUID})
	return f.principal, f.err
}

func principalContext(t *testing.T, ba *BotAPI, callerUID, callerKind, targetUID, spaceID string) (*wkhttp.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(w)
	gc.Request = httptest.NewRequest(http.MethodGet, "/v1/bot/space/principals/"+targetUID+"?space_id="+spaceID, nil)
	gc.Params = gin.Params{{Key: "uid", Value: targetUID}}
	c := &wkhttp.Context{Context: gc}
	c.Set(CtxKeyRobotID, callerUID)
	c.Set(CtxKeyBotKind, callerKind)
	return c, w
}

func TestBotSpacePrincipalHumanSuccessMinimalResponse(t *testing.T) {
	store := &fakeSpacePrincipalStore{principal: &spacePrincipal{UID: "human-1", PrincipalType: principalTypeHuman}}
	ba := &BotAPI{Log: log.NewTLog("principal-test"), principalStoreOverride: store}
	c, w := principalContext(t, ba, "bot-1", BotKindUser, "human-1", "space-1")

	ba.botSpacePrincipal(c)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Equal(t, map[string]any{"uid": "human-1", "principal_type": "human"}, got)
	require.Equal(t, [][4]string{{"bot-1", BotKindUser, "space-1", "human-1"}}, store.calls)
}

func TestBotSpacePrincipalValidation(t *testing.T) {
	for _, tc := range []struct {
		name, targetUID, spaceID string
	}{
		{name: "missing uid", spaceID: "space-1"},
		{name: "missing space", targetUID: "human-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeSpacePrincipalStore{}
			ba := &BotAPI{Log: log.NewTLog("principal-test"), principalStoreOverride: store}
			c, w := principalContext(t, ba, "bot-1", BotKindUser, tc.targetUID, tc.spaceID)
			ba.botSpacePrincipal(c)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			require.Empty(t, store.calls)
		})
	}
}

func TestBotSpacePrincipalRejectsAppBotCallerBeforeLookup(t *testing.T) {
	store := &fakeSpacePrincipalStore{}
	ba := &BotAPI{Log: log.NewTLog("principal-test"), principalStoreOverride: store}
	c, w := principalContext(t, ba, "app-bot-1", BotKindApp, "human-1", "space-1")

	ba.botSpacePrincipal(c)

	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), errcode.ErrBotAPIAppBotUnsupported.DefaultMessage)
	require.Empty(t, store.calls, "App Bot caller must be rejected before DB lookup")
}

func TestBotSpacePrincipalDenialsAreIndistinguishable(t *testing.T) {
	for _, name := range []string{
		"target absent", "caller outside space", "disabled space", "disabled member",
		"disabled bot", "bot creator absent",
	} {
		t.Run(name, func(t *testing.T) {
			store := &fakeSpacePrincipalStore{err: errSpacePrincipalNotFound}
			ba := &BotAPI{Log: log.NewTLog("principal-test"), principalStoreOverride: store}
			c, w := principalContext(t, ba, "bot-1", BotKindUser, "target-1", "space-1")
			ba.botSpacePrincipal(c)
			require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
		})
	}
}

func TestBotSpacePrincipalDBErrorUsesInternalEnvelope(t *testing.T) {
	store := &fakeSpacePrincipalStore{err: errors.New("database unavailable")}
	ba := &BotAPI{Log: log.NewTLog("principal-test"), principalStoreOverride: store}
	c, w := principalContext(t, ba, "bot-1", BotKindUser, "target-1", "space-1")
	ba.botSpacePrincipal(c)
	require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	require.NotContains(t, w.Body.String(), "database unavailable")
}

func TestBotSpacePrincipalRouteRegistered(t *testing.T) {
	source, err := os.ReadFile("bot_api.go")
	require.NoError(t, err)
	require.Contains(t, string(source), `botAPI.GET("/space/principals/:uid", ba.botSpacePrincipal)`)
}

func eligibleHumanRow() spacePrincipalEligibilityRow {
	return spacePrincipalEligibilityRow{
		SpaceActive: 1, CallerMember: 1, CallerUserBot: 1, CallerCreatorEligible: 1,
		CanonicalUID: "target-1", TargetMember: 1, TargetHuman: 1,
	}
}

func TestClassifyEligibleSpacePrincipal(t *testing.T) {
	userBot := eligibleHumanRow()
	userBot.TargetHuman, userBot.TargetRobotIdentity, userBot.TargetUserBot, userBot.TargetCreatorEligible = 0, 1, 1, 1
	appBot := eligibleHumanRow()
	appBot.TargetHuman, appBot.TargetAppBot, appBot.TargetCreatorEligible = 0, 1, 1

	for _, tc := range []struct {
		name, kind, want string
		row              spacePrincipalEligibilityRow
		ok               bool
	}{
		{name: "human", kind: BotKindUser, row: eligibleHumanRow(), want: principalTypeHuman, ok: true},
		{name: "human-looking App Bot rejected", kind: BotKindUser, row: func() spacePrincipalEligibilityRow { r := eligibleHumanRow(); r.TargetAppBot = 1; return r }()},
		{name: "user bot", kind: BotKindUser, row: userBot, want: principalTypeUserBot, ok: true},
		{name: "app bot target rejected", kind: BotKindUser, row: appBot},
		{name: "app bot caller rejected", kind: BotKindApp, row: eligibleHumanRow()},
		{name: "target absent", kind: BotKindUser, row: func() spacePrincipalEligibilityRow { r := eligibleHumanRow(); r.TargetHuman = 0; return r }()},
		{name: "caller outside exact space", kind: BotKindUser, row: func() spacePrincipalEligibilityRow { r := eligibleHumanRow(); r.CallerMember = 0; return r }()},
		{name: "caller user missing", kind: BotKindUser, row: func() spacePrincipalEligibilityRow { r := eligibleHumanRow(); r.CallerUserBot = 0; return r }()},
		{name: "caller user disabled", kind: BotKindUser, row: func() spacePrincipalEligibilityRow { r := eligibleHumanRow(); r.CallerUserBot = 0; return r }()},
		{name: "caller user destroyed", kind: BotKindUser, row: func() spacePrincipalEligibilityRow { r := eligibleHumanRow(); r.CallerUserBot = 0; return r }()},
		{name: "caller user robot flag false", kind: BotKindUser, row: func() spacePrincipalEligibilityRow { r := eligibleHumanRow(); r.CallerUserBot = 0; return r }()},
		{name: "caller creator is robot", kind: BotKindUser, row: func() spacePrincipalEligibilityRow { r := eligibleHumanRow(); r.CallerCreatorEligible = 0; return r }()},
		{name: "disabled space", kind: BotKindUser, row: func() spacePrincipalEligibilityRow { r := eligibleHumanRow(); r.SpaceActive = 0; return r }()},
		{name: "target outside exact space or disabled membership", kind: BotKindUser, row: func() spacePrincipalEligibilityRow { r := eligibleHumanRow(); r.TargetMember = 0; return r }()},
		{name: "target human disabled", kind: BotKindUser, row: func() spacePrincipalEligibilityRow { r := eligibleHumanRow(); r.TargetHuman = 0; return r }()},
		{name: "target user bot disabled", kind: BotKindUser, row: func() spacePrincipalEligibilityRow { r := userBot; r.TargetUserBot = 0; return r }()},
		{name: "bot creator absent", kind: BotKindUser, row: func() spacePrincipalEligibilityRow { r := userBot; r.TargetCreatorEligible = 0; return r }()},
		{name: "target creator is robot", kind: BotKindUser, row: func() spacePrincipalEligibilityRow { r := userBot; r.TargetCreatorEligible = 0; return r }()},
		{name: "caller creator absent", kind: BotKindUser, row: func() spacePrincipalEligibilityRow { r := eligibleHumanRow(); r.CallerCreatorEligible = 0; return r }()},
		{name: "conflicting bot identity", kind: BotKindUser, row: func() spacePrincipalEligibilityRow { r := userBot; r.TargetAppBot = 1; return r }()},
		{name: "human with robot identity", kind: BotKindUser, row: func() spacePrincipalEligibilityRow { r := eligibleHumanRow(); r.TargetRobotIdentity = 1; return r }()},
		{name: "ambiguous duplicate bot", kind: BotKindUser, row: func() spacePrincipalEligibilityRow { r := userBot; r.TargetUserBot = 2; return r }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := classifyEligibleSpacePrincipal(tc.kind, tc.row)
			if !tc.ok {
				require.ErrorIs(t, err, errSpacePrincipalNotFound)
				require.Nil(t, got)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got.PrincipalType)
			require.Equal(t, tc.row.CanonicalUID, got.UID)
		})
	}
}

func TestLookupEligibleSpacePrincipalDatabase(t *testing.T) {
	db, mock, closeDB := newSqlmockBotAPIDB(t)
	defer closeDB()

	columns := []string{
		"space_active", "caller_member", "caller_user_bot", "caller_app_bot",
		"caller_creator_eligible", "canonical_uid", "target_member",
		"target_human", "target_robot_identity", "target_user_bot", "target_app_bot", "target_creator_eligible",
	}
	mock.ExpectQuery(regexp.QuoteMeta(spacePrincipalEligibilityQuery)).
		WithArgs(
			"space-1", "space-1", "caller-bot", "caller-bot", "caller-bot", "space-1", "caller-bot",
			"HUMAN-1", "space-1", "HUMAN-1", "HUMAN-1", "HUMAN-1", "HUMAN-1", "HUMAN-1", "space-1", "HUMAN-1",
		).
		WillReturnRows(sqlmock.NewRows(columns).AddRow(1, 1, 1, 0, 1, "Human-1", 1, 1, 0, 0, 0, 0))

	got, err := db.lookupEligibleSpacePrincipal("caller-bot", BotKindUser, "space-1", "HUMAN-1")
	require.NoError(t, err)
	require.Equal(t, &spacePrincipal{UID: "Human-1", PrincipalType: principalTypeHuman}, got)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSpacePrincipalEligibilitySQLArgsAreExactSpaceScoped(t *testing.T) {
	want := []interface{}{
		"space-1",
		"space-1", "caller-bot",
		"caller-bot",
		"caller-bot",
		"space-1", "caller-bot",
		"target-1",
		"space-1", "target-1",
		"target-1",
		"target-1",
		"target-1",
		"target-1",
		"space-1", "target-1",
	}
	require.Equal(t, want, spacePrincipalEligibilityArgs("caller-bot", "space-1", "target-1"))
	require.Equal(t, len(want), strings.Count(spacePrincipalEligibilityQuery, "?"))
	require.NotContains(t, spacePrincipalEligibilityQuery, "scope='platform'")
	require.NotContains(t, spacePrincipalEligibilityQuery, "COLLATE")
	require.Contains(t, spacePrincipalEligibilityQuery, "SELECT u.uid FROM user u WHERE u.uid=? LIMIT 1")
	require.Contains(t, spacePrincipalEligibilityQuery, "FROM app_bot ab WHERE ab.uid=?) AS target_app_bot")
}

func TestSpacePrincipalEligibilitySQLRequiresCompleteUserBotIdentitiesAndHumanCreators(t *testing.T) {
	for _, tc := range []struct {
		name, fragment string
	}{
		{name: "caller user missing", fragment: "FROM robot r JOIN user u ON u.uid=r.robot_id"},
		{name: "caller user disabled", fragment: "u.uid=r.robot_id AND u.status=1"},
		{name: "caller user destroyed", fragment: "u.status=1 AND COALESCE(u.is_destroy,0)<>2"},
		{name: "caller user robot flag false", fragment: "COALESCE(u.is_destroy,0)<>2 AND u.robot=1 WHERE r.robot_id=? AND r.status=1) AS caller_user_bot"},
		{name: "target User Bot symmetric identity", fragment: "COALESCE(u.is_destroy,0)<>2 AND u.robot=1 WHERE r.robot_id=? AND r.status=1) AS target_user_bot"},
		{name: "caller creator is robot", fragment: "COALESCE(u.is_destroy,0)<>2 AND u.robot=0 JOIN space_member sm ON sm.uid=r.creator_uid AND sm.space_id=? AND sm.status=1 WHERE r.robot_id=? AND r.status=1) AS caller_creator_eligible"},
		{name: "target creator is robot", fragment: "COALESCE(u.is_destroy,0)<>2 AND u.robot=0 JOIN space_member sm ON sm.uid=r.creator_uid AND sm.space_id=? AND sm.status=1 WHERE r.robot_id=? AND r.status=1) AS target_creator_eligible"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Contains(t, spacePrincipalEligibilityQuery, tc.fragment)
		})
	}
}

func TestLookupEligibleSpacePrincipalDatabaseError(t *testing.T) {
	db, mock, closeDB := newSqlmockBotAPIDB(t)
	defer closeDB()

	wantErr := errors.New("query failed")
	mock.ExpectQuery(regexp.QuoteMeta(spacePrincipalEligibilityQuery)).
		WithArgs(
			"space-1", "space-1", "caller-bot", "caller-bot", "caller-bot", "space-1", "caller-bot",
			"human-1", "space-1", "human-1", "human-1", "human-1", "human-1", "human-1", "space-1", "human-1",
		).
		WillReturnError(wantErr)

	got, err := db.lookupEligibleSpacePrincipal("caller-bot", BotKindUser, "space-1", "human-1")
	require.ErrorIs(t, err, wantErr)
	require.Nil(t, got)
	require.NoError(t, mock.ExpectationsWereMet())
}
