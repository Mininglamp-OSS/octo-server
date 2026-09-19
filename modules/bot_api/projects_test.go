package bot_api

// =============================================================================
// bot-project-api — GET /v1/bot/projects 与 GET /v1/bot/projects/:project_id
//
// 覆盖：
//   - 可见范围：owner（robot.creator_uid）有效席位 ∪ bot 自身有效席位；同 Space
//     无席位项目、跨 Space 项目、已解散项目不可见；bot 在目标 Space 无席位时
//     list 拒绝；席位只在持有人仍是该 Space 的有效成员期间算数（owner 被移出
//     Space / 账号注销后，残留的席位行不再授权）。
//   - 投影与分页：与人类侧同形的 Resp 数组 + X-Total-Count；keyword 过滤；
//     limit/page 翻页；my_role 取席位优先级（bot 自己优先，其次 owner）；
//     bot 自己的置顶行生效；空结果是 [] 而不是 null。
//   - 失败语义：无 token → 401；缺 space_id → request_invalid；App Bot →
//     app_bot_unsupported；get 的一切不可见折叠成同一个 project.not_found（404）；
//     非法 page/limit 不产生 500。
//
// 依赖 MySQL + Redis（authBot 查 robot.bot_token / app_bot.token）。
// =============================================================================

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	projectmod "github.com/Mininglamp-OSS/octo-server/modules/project"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	bpaSpaceA = "space_bpa_a"
	bpaSpaceB = "space_bpa_b"

	// bpaOwnerUID is a human member of BOTH Spaces: Space B exists so the
	// cross-Space case has a project the owner legitimately belongs to while the
	// bot does not.
	bpaOwnerUID = "owner_bpa"

	bpaBotID    = "bot_bpa_1"
	bpaBotToken = "bf_bpa_token_1"

	// bpaOrphanBotID has no creator_uid, so its seat set degenerates to itself.
	bpaOrphanBotID    = "bot_bpa_orphan"
	bpaOrphanBotToken = "bf_bpa_orphan_token"

	bpaAppBotUID   = "appbot_bpa_1"
	bpaAppBotToken = "app_bpa_token_1"

	// Projects in Space A: owner seat only / bot seat only / no seat at all /
	// a seat for the orphan bot.
	bpaProjectOwnerSeat  = "proj_bpa_owner_seat"
	bpaProjectBotSeat    = "proj_bpa_bot_seat"
	bpaProjectNoSeat     = "proj_bpa_no_seat"
	bpaProjectOrphanSeat = "proj_bpa_orphan_seat"
	// bpaProjectBothSeats is seated to BOTH the bot and its owner.
	bpaProjectBothSeats = "proj_bpa_both_seats"
	// bpaProjectOtherSpace lives in Space B; the owner holds a seat there.
	bpaProjectOtherSpace = "proj_bpa_other_space"

	bpaProjectMissing = "proj_bpa_missing"
)

// bpaSetup wires a real BotAPI on a clean DB.
func bpaSetup(t *testing.T) (http.Handler, *config.Context) {
	t.Helper()
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	route := s.GetRoute()
	// Localized envelope, so error assertions can name the registered code rather
	// than only the rendered message.
	route.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))

	bpaInsertSpace(t, ctx, bpaSpaceA)
	bpaInsertSpace(t, ctx, bpaSpaceB)

	// The owner: an active human member of both Spaces.
	bpaInsertUser(t, ctx, bpaOwnerUID, 0)
	bpaInsertSpaceMember(t, ctx, bpaSpaceA, bpaOwnerUID)
	bpaInsertSpaceMember(t, ctx, bpaSpaceB, bpaOwnerUID)

	// The bot: user(robot=1) + robot(status=1, creator=owner) + Space seat in A.
	bpaInsertUser(t, ctx, bpaBotID, 1)
	bpaInsertRobot(t, ctx, bpaBotID, bpaBotToken, bpaOwnerUID)
	bpaInsertSpaceMember(t, ctx, bpaSpaceA, bpaBotID)

	// The orphan bot: no creator_uid.
	bpaInsertUser(t, ctx, bpaOrphanBotID, 1)
	bpaInsertRobot(t, ctx, bpaOrphanBotID, bpaOrphanBotToken, "")
	bpaInsertSpaceMember(t, ctx, bpaSpaceA, bpaOrphanBotID)

	// The app_bot table is not part of this package's migration set (modules/app_bot
	// is not imported here), so the existing suite helper creates it on demand.
	ensureAppBotTable(t, ctx)
	bpaInsertAppBot(t, ctx, bpaAppBotUID, bpaAppBotToken, bpaSpaceA)

	bpaInsertProject(t, ctx, bpaProjectOwnerSeat, bpaSpaceA, "bpa alpha", bpaOwnerUID)
	bpaInsertSeat(t, ctx, bpaProjectOwnerSeat, bpaSpaceA, bpaOwnerUID, projectmod.RoleOwner)
	bpaInsertProject(t, ctx, bpaProjectBotSeat, bpaSpaceA, "bpa beta", bpaBotID)
	bpaInsertSeat(t, ctx, bpaProjectBotSeat, bpaSpaceA, bpaBotID, projectmod.RoleCommon)
	bpaInsertProject(t, ctx, bpaProjectNoSeat, bpaSpaceA, "bpa gamma", bpaOwnerUID)
	bpaInsertProject(t, ctx, bpaProjectOrphanSeat, bpaSpaceA, "bpa epsilon", bpaOrphanBotID)
	bpaInsertSeat(t, ctx, bpaProjectOrphanSeat, bpaSpaceA, bpaOrphanBotID, projectmod.RoleCommon)
	bpaInsertProject(t, ctx, bpaProjectOtherSpace, bpaSpaceB, "bpa delta", bpaOwnerUID)
	bpaInsertSeat(t, ctx, bpaProjectOtherSpace, bpaSpaceB, bpaOwnerUID, projectmod.RoleOwner)

	return route, ctx
}

func bpaInsertSpace(t *testing.T, ctx *config.Context, spaceID string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `space` (space_id, name, creator, status) VALUES (?, ?, 'creator_bpa', 1)",
		spaceID, spaceID,
	).Exec()
	require.NoError(t, err)
}

func bpaInsertUser(t *testing.T, ctx *config.Context, uid string, robot int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `user` (uid, name, short_no, robot, status) VALUES (?, ?, ?, ?, 1)",
		uid, "name-"+uid, "sn_"+uid, robot,
	).Exec()
	require.NoError(t, err)
}

func bpaInsertRobot(t *testing.T, ctx *config.Context, robotID, token, creator string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, status, creator_uid, bot_token) VALUES (?, 1, ?, ?)",
		robotID, creator, token,
	).Exec()
	require.NoError(t, err)
}

func bpaInsertSpaceMember(t *testing.T, ctx *config.Context, spaceID, uid string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, 0, 1)",
		spaceID, uid,
	).Exec()
	require.NoError(t, err)
}

func bpaInsertAppBot(t *testing.T, ctx *config.Context, uid, token, spaceID string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO app_bot (id, uid, display_name, scope, space_id, status, token, created_by) "+
			"VALUES (?, ?, ?, 'space', ?, 1, ?, 'admin')",
		uid, uid, "App Bot "+uid, spaceID, token,
	).Exec()
	require.NoError(t, err)
}

func bpaInsertProject(t *testing.T, ctx *config.Context, projectID, spaceID, name, creator string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO octo_project (project_id, space_id, name, creator, status, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, 1, NOW(3), NOW(3))",
		projectID, spaceID, name, creator,
	).Exec()
	require.NoError(t, err)
}

func bpaInsertSeat(t *testing.T, ctx *config.Context, projectID, spaceID, uid string, role int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO octo_project_member "+
			"(project_id, uid, space_id, role, status, invite_uid, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, 1, '', NOW(3), NOW(3))",
		projectID, uid, spaceID, role,
	).Exec()
	require.NoError(t, err)
}

func bpaInsertPin(t *testing.T, ctx *config.Context, projectID, uid string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `octo_project_user_setting` (project_id, uid, pinned, pinned_at, created_at, updated_at) "+
			"VALUES (?, ?, 1, NOW(3), NOW(3), NOW(3))",
		projectID, uid,
	).Exec()
	require.NoError(t, err)
}

func bpaGet(t *testing.T, handler http.Handler, token, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodGet, path, nil)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	handler.ServeHTTP(w, req)
	return w
}

func bpaDecodeList(t *testing.T, w *httptest.ResponseRecorder) []*projectmod.Resp {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	var rows []*projectmod.Resp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rows), "body=%s", w.Body.String())
	return rows
}

func bpaDecodeErr(t *testing.T, w *httptest.ResponseRecorder) errEnvelope {
	t.Helper()
	var env errEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env), "body=%s", w.Body.String())
	return env
}

// TestBotProjectRoutesRequireAuthAndRejectAppBot pins the route registration
// (a missing route answers 404, an auth failure answers 401) and the User-Bot-only
// boundary.
func TestBotProjectRoutesRequireAuthAndRejectAppBot(t *testing.T) {
	handler, _ := bpaSetup(t)

	for _, path := range []string{"/v1/bot/projects?space_id=" + bpaSpaceA, "/v1/bot/projects/" + bpaProjectBotSeat} {
		w := bpaGet(t, handler, "", path)
		require.Equal(t, http.StatusUnauthorized, w.Code, "path=%s body=%s", path, w.Body.String())
		assert.Equal(t, errcode.ErrBotAPIAuthFailed.ID, bpaDecodeErr(t, w).Error.Code, "path=%s", path)
	}

	for _, path := range []string{"/v1/bot/projects?space_id=" + bpaSpaceA, "/v1/bot/projects/" + bpaProjectBotSeat} {
		w := bpaGet(t, handler, bpaAppBotToken, path)
		require.Equal(t, http.StatusForbidden, w.Code, "path=%s body=%s", path, w.Body.String())
		assert.Equal(t, errcode.ErrBotAPIAppBotUnsupported.ID, bpaDecodeErr(t, w).Error.Code, "path=%s", path)
	}
}

// TestBotProjectListVisibilityShapeAndOrder is the core case: the owner's seat
// makes a project visible to the bot, the bot's own seat also does, and nothing
// else in the Space does. The projection is the user-facing Resp, with my_role
// resolved by seat precedence.
func TestBotProjectListVisibilityShapeAndOrder(t *testing.T) {
	handler, _ := bpaSetup(t)

	w := bpaGet(t, handler, bpaBotToken, "/v1/bot/projects?space_id="+bpaSpaceA)
	rows := bpaDecodeList(t, w)
	assert.Equal(t, "2", w.Header().Get("X-Total-Count"))
	require.Len(t, rows, 2)

	// Order is p.id DESC (no pins for a bot), so the later-inserted project first.
	visible := map[string]*projectmod.Resp{}
	for _, row := range rows {
		visible[row.ProjectID] = row
	}
	_, hasNoSeat := visible[bpaProjectNoSeat]
	_, hasOtherSpace := visible[bpaProjectOtherSpace]
	_, hasOrphanSeat := visible[bpaProjectOrphanSeat]
	assert.False(t, hasNoSeat, "project without any seat must not be listed")
	assert.False(t, hasOtherSpace, "project in another Space must not be listed")
	assert.False(t, hasOrphanSeat, "project seated to another bot must not be listed")

	// Owner seat only: visible through the owner, my_role is the OWNER's role
	// (the bot itself has no seat), and the roster counts are the project's.
	ownerSeat := visible[bpaProjectOwnerSeat]
	require.NotNil(t, ownerSeat, "owner-seated project must be listed; got %s", w.Body.String())
	assert.Equal(t, "bpa alpha", ownerSeat.Name)
	assert.Equal(t, bpaSpaceA, ownerSeat.SpaceID)
	assert.Equal(t, projectmod.RoleOwner, ownerSeat.MyRole)
	assert.Equal(t, 1, ownerSeat.MemberCount)
	assert.Equal(t, 1, ownerSeat.HumanMemberCount)
	assert.Equal(t, 0, ownerSeat.AgentMemberCount)
	assert.Equal(t, 1, ownerSeat.Status)

	// Bot seat only: the bot's OWN role wins, and the seat is an agent seat.
	botSeat := visible[bpaProjectBotSeat]
	require.NotNil(t, botSeat, "bot-seated project must be listed; got %s", w.Body.String())
	assert.Equal(t, projectmod.RoleCommon, botSeat.MyRole)
	assert.Equal(t, 1, botSeat.MemberCount)
	assert.Equal(t, 0, botSeat.HumanMemberCount)
	assert.Equal(t, 1, botSeat.AgentMemberCount)
}

// TestBotProjectListKeywordPagingAndHostileParams covers the pagination contract
// (same page/limit semantics and X-Total-Count as the user-facing list) and that
// a hostile page/limit degrades instead of producing a 500.
func TestBotProjectListKeywordPagingAndHostileParams(t *testing.T) {
	handler, _ := bpaSetup(t)

	w := bpaGet(t, handler, bpaBotToken, "/v1/bot/projects?space_id="+bpaSpaceA+"&keyword=alpha")
	rows := bpaDecodeList(t, w)
	require.Len(t, rows, 1)
	assert.Equal(t, bpaProjectOwnerSeat, rows[0].ProjectID)
	assert.Equal(t, "1", w.Header().Get("X-Total-Count"))

	first := bpaDecodeList(t, bpaGet(t, handler, bpaBotToken,
		"/v1/bot/projects?space_id="+bpaSpaceA+"&limit=1&page=1"))
	require.Len(t, first, 1)
	assert.Equal(t, bpaProjectBotSeat, first[0].ProjectID)

	w = bpaGet(t, handler, bpaBotToken, "/v1/bot/projects?space_id="+bpaSpaceA+"&limit=1&page=2")
	second := bpaDecodeList(t, w)
	require.Len(t, second, 1)
	assert.Equal(t, bpaProjectOwnerSeat, second[0].ProjectID)
	assert.Equal(t, "2", w.Header().Get("X-Total-Count"), "total counts every visible project, not the page")

	w = bpaGet(t, handler, bpaBotToken, "/v1/bot/projects?space_id="+bpaSpaceA+"&limit=1&page=3")
	assert.Empty(t, bpaDecodeList(t, w))
	assert.Equal(t, "2", w.Header().Get("X-Total-Count"))

	// Negative limit would reach MySQL as `LIMIT -5` (1064 → 500) if the clamp
	// lived anywhere but before the offset computation.
	hostile := bpaDecodeList(t, bpaGet(t, handler, bpaBotToken,
		"/v1/bot/projects?space_id="+bpaSpaceA+"&limit=-5&page=-3"))
	assert.Len(t, hostile, 2)

	// MaxInt64 page: clamped to the module's page cap, so (page-1)*limit cannot
	// overflow into a negative OFFSET.
	w = bpaGet(t, handler, bpaBotToken, "/v1/bot/projects?space_id="+bpaSpaceA+"&page=9223372036854775807")
	assert.Empty(t, bpaDecodeList(t, w))
	assert.Equal(t, "2", w.Header().Get("X-Total-Count"))

	// Unparsable values fall back to the defaults rather than erroring.
	assert.Len(t, bpaDecodeList(t, bpaGet(t, handler, bpaBotToken,
		"/v1/bot/projects?space_id="+bpaSpaceA+"&limit=abc&page=xyz")), 2)
}

// TestBotProjectGetMatchesListAndCollapsesInvisible pins the detail contract: the
// Space is derived, a visible project matches its list projection, and every
// invisible case is byte-identical (no existence oracle).
func TestBotProjectGetMatchesListAndCollapsesInvisible(t *testing.T) {
	handler, _ := bpaSetup(t)

	listRows := bpaDecodeList(t, bpaGet(t, handler, bpaBotToken, "/v1/bot/projects?space_id="+bpaSpaceA))
	w := bpaGet(t, handler, bpaBotToken, "/v1/bot/projects/"+bpaProjectOwnerSeat)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	var got projectmod.Resp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	var want *projectmod.Resp
	for _, row := range listRows {
		if row.ProjectID == bpaProjectOwnerSeat {
			want = row
		}
	}
	require.NotNil(t, want)
	assert.Equal(t, *want, got, "detail must return the same object the list returns")

	invisible := []string{
		bpaProjectNoSeat,     // in the Space, no seat for bot or owner
		bpaProjectOtherSpace, // owner-seated but in a Space the bot is not in
		bpaProjectOrphanSeat, // seated to another bot
		bpaProjectMissing,    // no such project
	}
	var bodies []string
	for _, projectID := range invisible {
		w := bpaGet(t, handler, bpaBotToken, "/v1/bot/projects/"+projectID)
		require.Equal(t, http.StatusNotFound, w.Code, "project=%s body=%s", projectID, w.Body.String())
		assert.Equal(t, errcode.ErrProjectNotFound.ID, bpaDecodeErr(t, w).Error.Code, "project=%s", projectID)
		bodies = append(bodies, w.Body.String())
	}
	for i := 1; i < len(bodies); i++ {
		assert.Equal(t, bodies[0], bodies[i], "every invisible case must render byte-identically")
	}
}

// TestBotProjectListRejectsForeignMissingOrInactiveSpace covers the list-side
// refusals: the caller names the Space, so "not my Space" is an actionable 403
// rather than a silent empty list.
func TestBotProjectListRejectsForeignMissingOrInactiveSpace(t *testing.T) {
	handler, ctx := bpaSetup(t)

	for _, spaceID := range []string{bpaSpaceB, "space_bpa_nonexistent"} {
		w := bpaGet(t, handler, bpaBotToken, "/v1/bot/projects?space_id="+spaceID)
		require.Equal(t, http.StatusForbidden, w.Code, "space=%s body=%s", spaceID, w.Body.String())
		assert.Equal(t, errcode.ErrBotAPINotSpaceMember.ID, bpaDecodeErr(t, w).Error.Code, "space=%s", spaceID)
		assert.Empty(t, w.Header().Get("X-Total-Count"), "a refused list must not carry a count")
	}

	w := bpaGet(t, handler, bpaBotToken, "/v1/bot/projects")
	require.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	env := bpaDecodeErr(t, w)
	assert.Equal(t, errcode.ErrBotAPIRequestInvalid.ID, env.Error.Code)
	assert.Equal(t, "space_id", env.Error.Details["field"])

	// An inactive Space is the same refusal as "not a member": the Space gate is
	// the Space's own status AND the caller's seat.
	_, err := ctx.DB().UpdateBySql("UPDATE `space` SET status = 0 WHERE space_id = ?", bpaSpaceA).Exec()
	require.NoError(t, err)
	w = bpaGet(t, handler, bpaBotToken, "/v1/bot/projects?space_id="+bpaSpaceA)
	require.Equal(t, http.StatusForbidden, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, errcode.ErrBotAPINotSpaceMember.ID, bpaDecodeErr(t, w).Error.Code)
}

// TestBotProjectListDoesNotDuplicateProjectsSeatedToBothBotAndOwner pins the
// dedup property of the two-join seat predicate: octo_project_member's primary
// key is (project_id, uid), so a project where BOTH the bot and its owner hold a
// seat must appear exactly once — and the bot's own role wins.
func TestBotProjectListDoesNotDuplicateProjectsSeatedToBothBotAndOwner(t *testing.T) {
	handler, ctx := bpaSetup(t)
	bpaInsertProject(t, ctx, bpaProjectBothSeats, bpaSpaceA, "bpa zeta", bpaOwnerUID)
	bpaInsertSeat(t, ctx, bpaProjectBothSeats, bpaSpaceA, bpaOwnerUID, projectmod.RoleOwner)
	bpaInsertSeat(t, ctx, bpaProjectBothSeats, bpaSpaceA, bpaBotID, projectmod.RoleCommon)

	w := bpaGet(t, handler, bpaBotToken, "/v1/bot/projects?space_id="+bpaSpaceA)
	rows := bpaDecodeList(t, w)
	assert.Equal(t, "3", w.Header().Get("X-Total-Count"))
	hits := 0
	var both *projectmod.Resp
	for _, row := range rows {
		if row.ProjectID == bpaProjectBothSeats {
			hits++
			both = row
		}
	}
	require.Equal(t, 1, hits, "a project seated to both uids must not be multiplied by the joins")
	require.NotNil(t, both)
	assert.Equal(t, projectmod.RoleCommon, both.MyRole, "the bot's own seat outranks its owner's")
	assert.Equal(t, 2, both.MemberCount)

	detail := bpaGet(t, handler, bpaBotToken, "/v1/bot/projects/"+bpaProjectBothSeats)
	require.Equal(t, http.StatusOK, detail.Code, "body=%s", detail.Body.String())
	var got projectmod.Resp
	require.NoError(t, json.Unmarshal(detail.Body.Bytes(), &got))
	assert.Equal(t, *both, got, "detail must agree with the list row on the same seat set")
}

// TestBotProjectVisibilityDoesNotOutliveTheOwnerSeat pins the liveness check on
// every seat uid: an `octo_project_member` row only counts while its holder is
// still an eligible, active member of the Project's Space.
//
// Both states below are reachable in production with the seat row untouched:
// removing a member from a Space closes their Project seats asynchronously (and a
// work order that never converges leaves them open), while destroying an account
// closes neither `robot` nor the seat tables at all. Without the check the
// owner's revocation would EXTEND the bot's read window past the owner's own:
// the bot would keep reading Projects its owner can no longer read.
func TestBotProjectVisibilityDoesNotOutliveTheOwnerSeat(t *testing.T) {
	handler, ctx := bpaSetup(t)

	baseline := bpaDecodeList(t, bpaGet(t, handler, bpaBotToken, "/v1/bot/projects?space_id="+bpaSpaceA))
	require.Len(t, baseline, 2, "owner-seated and bot-seated projects are both visible")

	// Owner removed from the Space: `space_member` closed, Project seat still
	// active — the window between the removal and the seat cascade running.
	_, err := ctx.DB().UpdateBySql(
		"UPDATE space_member SET status = 0 WHERE space_id = ? AND uid = ?", bpaSpaceA, bpaOwnerUID,
	).Exec()
	require.NoError(t, err)

	w := bpaGet(t, handler, bpaBotToken, "/v1/bot/projects?space_id="+bpaSpaceA)
	rows := bpaDecodeList(t, w)
	require.Len(t, rows, 1, "only the bot's own seat survives: %s", w.Body.String())
	assert.Equal(t, bpaProjectBotSeat, rows[0].ProjectID)
	assert.Equal(t, "1", w.Header().Get("X-Total-Count"))

	detail := bpaGet(t, handler, bpaBotToken, "/v1/bot/projects/"+bpaProjectOwnerSeat)
	require.Equal(t, http.StatusNotFound, detail.Code, "body=%s", detail.Body.String())
	assert.Equal(t, errcode.ErrProjectNotFound.ID, bpaDecodeErr(t, detail).Error.Code)

	// Live state, not a one-way latch: restoring the Space seat restores the
	// owner's visibility through the bot.
	_, err = ctx.DB().UpdateBySql(
		"UPDATE space_member SET status = 1 WHERE space_id = ? AND uid = ?", bpaSpaceA, bpaOwnerUID,
	).Exec()
	require.NoError(t, err)
	assert.Len(t, bpaDecodeList(t, bpaGet(t, handler, bpaBotToken, "/v1/bot/projects?space_id="+bpaSpaceA)), 2)

	// Owner account destroyed, Space seat left behind: the owner's own reads fail
	// the eligibility gate, and the bot must not be a way around that.
	_, err = ctx.DB().UpdateBySql("UPDATE `user` SET is_destroy = 2 WHERE uid = ?", bpaOwnerUID).Exec()
	require.NoError(t, err)

	w = bpaGet(t, handler, bpaBotToken, "/v1/bot/projects?space_id="+bpaSpaceA)
	rows = bpaDecodeList(t, w)
	require.Len(t, rows, 1, "a destroyed owner grants nothing: %s", w.Body.String())
	assert.Equal(t, bpaProjectBotSeat, rows[0].ProjectID)
	assert.Equal(t, http.StatusNotFound,
		bpaGet(t, handler, bpaBotToken, "/v1/bot/projects/"+bpaProjectOwnerSeat).Code)
}

// TestBotProjectListHidesDissolvedProjectsAndReturnsAnEmptyArray pins the two
// projection edges: a dissolved Project (`octo_project.status`) leaves the list
// and 404s on detail like every other invisible case, and an empty page is `[]`,
// never `null`.
func TestBotProjectListHidesDissolvedProjectsAndReturnsAnEmptyArray(t *testing.T) {
	handler, ctx := bpaSetup(t)

	_, err := ctx.DB().UpdateBySql(
		"UPDATE octo_project SET status = 0 WHERE project_id = ?", bpaProjectBotSeat,
	).Exec()
	require.NoError(t, err)

	dissolved := bpaGet(t, handler, bpaBotToken, "/v1/bot/projects/"+bpaProjectBotSeat)
	require.Equal(t, http.StatusNotFound, dissolved.Code, "body=%s", dissolved.Body.String())
	assert.Equal(t, errcode.ErrProjectNotFound.ID, bpaDecodeErr(t, dissolved).Error.Code)

	w := bpaGet(t, handler, bpaBotToken, "/v1/bot/projects?space_id="+bpaSpaceA+"&keyword=beta")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, "[]", strings.TrimSpace(w.Body.String()), "an empty result must marshal as []")
	assert.Equal(t, "0", w.Header().Get("X-Total-Count"))
}

// TestBotProjectListCarriesTheBotsOwnPin pins the pin wiring: pinUID is the
// caller, spliced between the seat-join arguments and the WHERE arguments, so a
// bot's own `octo_project_user_setting` row must surface as `pinned` and sort
// first — and another bot's row must not.
func TestBotProjectListCarriesTheBotsOwnPin(t *testing.T) {
	handler, ctx := bpaSetup(t)
	bpaInsertPin(t, ctx, bpaProjectOwnerSeat, bpaBotID)
	bpaInsertPin(t, ctx, bpaProjectBotSeat, bpaOrphanBotID)

	rows := bpaDecodeList(t, bpaGet(t, handler, bpaBotToken, "/v1/bot/projects?space_id="+bpaSpaceA))
	require.Len(t, rows, 2)
	assert.Equal(t, bpaProjectOwnerSeat, rows[0].ProjectID, "the caller's pinned project sorts first")
	assert.True(t, rows[0].Pinned)
	assert.False(t, rows[1].Pinned, "another bot's pin must not mark this bot's row")
}

// TestBotProjectListOrphanBotSeesOwnSeatOnly covers the degenerate seat set: a
// bot with no creator_uid reads through its own seats and nothing else, on both
// entry points.
func TestBotProjectListOrphanBotSeesOwnSeatOnly(t *testing.T) {
	handler, _ := bpaSetup(t)

	rows := bpaDecodeList(t, bpaGet(t, handler, bpaOrphanBotToken, "/v1/bot/projects?space_id="+bpaSpaceA))
	require.Len(t, rows, 1)
	assert.Equal(t, bpaProjectOrphanSeat, rows[0].ProjectID)

	detail := bpaGet(t, handler, bpaOrphanBotToken, "/v1/bot/projects/"+bpaProjectOrphanSeat)
	require.Equal(t, http.StatusOK, detail.Code, "body=%s", detail.Body.String())
	var got projectmod.Resp
	require.NoError(t, json.Unmarshal(detail.Body.Bytes(), &got))
	assert.Equal(t, *rows[0], got, "detail must agree with the list row")
	assert.Equal(t, http.StatusNotFound,
		bpaGet(t, handler, bpaOrphanBotToken, "/v1/bot/projects/"+bpaProjectOwnerSeat).Code)

	// ...while the owner-linked bot does not see the orphan's project.
	rows = bpaDecodeList(t, bpaGet(t, handler, bpaBotToken, "/v1/bot/projects?space_id="+bpaSpaceA))
	for _, row := range rows {
		assert.NotEqual(t, bpaProjectOrphanSeat, row.ProjectID)
	}
}
