package space

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// ---------- test fixtures ----------

type directoryItemForTest struct {
	UID       string          `json:"uid"`
	Name      string          `json:"name"`
	Kind      string          `json:"kind"`
	Role      int             `json:"role"`
	CreatedAt string          `json:"created_at"`
	Bot       *directoryBotFT `json:"bot"`
}

type directoryBotFT struct {
	Relation string `json:"relation"`
}

type directoryRespForTest struct {
	Total int                    `json:"total"`
	Page  int                    `json:"page"`
	Limit int                    `json:"limit"`
	Items []directoryItemForTest `json:"items"`
}

func getDirectory(t *testing.T, srv *server.Server, ctx *config.Context, spaceID string, q url.Values) *httptest.ResponseRecorder {
	t.Helper()
	resetSpaceUIDRateLimit(t, ctx)
	path := "/v1/space/" + spaceID + "/directory"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("token", testutil.Token)
	srv.GetRoute().ServeHTTP(w, req)
	return w
}

func decodeDirectory(t *testing.T, w *httptest.ResponseRecorder) directoryRespForTest {
	t.Helper()
	var resp directoryRespForTest
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	return resp
}

func directoryQuery(listType string, page, limit int) url.Values {
	q := url.Values{}
	if listType != "" {
		q.Set("type", listType)
	}
	if page > 0 {
		q.Set("page", fmt.Sprintf("%d", page))
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}
	return q
}

func directoryUIDs(resp directoryRespForTest) []string {
	uids := make([]string, 0, len(resp.Items))
	for _, item := range resp.Items {
		uids = append(uids, item.UID)
	}
	return uids
}

// seedDirectorySpace 建一个空间，并把 creator 设为拥有者成员。
func seedDirectorySpace(t *testing.T, spaceID, creator string) {
	t.Helper()
	assert.NoError(t, testSpaceDB.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceID,
		Name:    spaceID,
		Creator: creator,
		Status:  1,
	}))
	seedDirectoryHuman(t, spaceID, creator, "Owner", 2)
}

// seedDirectoryUser 写一条 user 行。robot=1 表示 bot 账号。
func seedDirectoryUser(t *testing.T, uid, name string, robot int) {
	t.Helper()
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO `user` (uid, name, robot) VALUES (?, ?, ?)", uid, name, robot,
	).Exec()
	assert.NoError(t, err)
}

func seedDirectoryMember(t *testing.T, spaceID, uid string, role int) {
	t.Helper()
	assert.NoError(t, testSpaceDB.insertMemberNoTx(&MemberModel{
		SpaceId: spaceID,
		UID:     uid,
		Role:    role,
		Status:  1,
	}))
}

func seedDirectoryHuman(t *testing.T, spaceID, uid, name string, role int) {
	t.Helper()
	seedDirectoryUser(t, uid, name, 0)
	seedDirectoryMember(t, spaceID, uid, role)
}

// seedDirectoryBot 建一个 bot：user.robot=1 + space_member + robot 行。
// status 传 1 为启用，其它值模拟停用 bot。
func seedDirectoryBot(t *testing.T, spaceID, uid, name, creatorUID string, status int) {
	t.Helper()
	seedDirectoryUser(t, uid, name, 1)
	seedDirectoryMember(t, spaceID, uid, 0)
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, status, creator_uid) VALUES (?, ?, ?)", uid, status, creatorUID,
	).Exec()
	assert.NoError(t, err)
}

// ---------- isolation ----------

// TestDirectory_NonMemberRefused 是这个接口的核心安全断言：目录会把整个 Space 的
// 花名册（含全部 bot）交出去，非成员必须一行都拿不到。断言落在响应体上而不是只看
// 状态码——状态码对了但 items 非空同样是越权。
func TestDirectory_NonMemberRefused(t *testing.T) {
	srv, _, err := setup(t)
	assert.NoError(t, err)

	spaceID := "dir_space_outsider"
	// 空间的创建者是别人；testutil.UID（token 身份）不是成员。
	seedDirectorySpace(t, spaceID, "someone_else")
	seedDirectoryHuman(t, spaceID, "member_a", "Member A", 0)
	seedDirectoryBot(t, spaceID, "bot_a", "Bot A", "someone_else", 1)

	w := getDirectory(t, srv, testCtx, spaceID, directoryQuery("", 0, 0))

	// 断言必须能证伪。把错误体反序列化进 directoryRespForTest 后再断言
	// Items 为空、Total 为 0，在**任何**非 200 响应下都恒成立，等于没断言——
	// 所以这里直接钉状态码，并断言响应体里不出现任何 fixture 的 uid：
	// 修复被回滚（拿掉 SpaceMiddleware）时会返回 200 + 完整名册，两条都会失败。
	assert.Equal(t, http.StatusForbidden, w.Code, "非成员必须被拒: %s", w.Body.String())
	for _, leaked := range []string{"member_a", "bot_a", "Member A", "Bot A"} {
		assert.NotContains(t, w.Body.String(), leaked, "响应体不得泄露成员/bot 信息")
	}
}

// TestDirectory_BotVisibilityIsCreatorIndependent 钉住本接口与 listMembers 的
// 刻意分歧：别人创建的 bot 在目录里可见，在成员列表里不可见。两个断言放在同一个
// 用例里，任何一边被改动都会立刻暴露。
func TestDirectory_BotVisibilityIsCreatorIndependent(t *testing.T) {
	srv, _, err := setup(t)
	assert.NoError(t, err)

	spaceID := "dir_space_creator"
	seedDirectorySpace(t, spaceID, testutil.UID)
	// bot 由别人创建，调用者只是同空间成员。
	seedDirectoryBot(t, spaceID, "bot_foreign", "Foreign Bot", "another_creator", 1)

	resp := decodeDirectory(t, getDirectory(t, srv, testCtx, spaceID, directoryQuery(directoryTypeBot, 0, 0)))
	assert.Contains(t, directoryUIDs(resp), "bot_foreign", "目录必须返回别人创建的 bot")

	// 同一个 bot 在 listMembers 的 creator 过滤下不可见。
	members, err := testSpaceDB.queryMembers(spaceID, testutil.UID, 1, 100)
	assert.NoError(t, err)
	for _, m := range members {
		assert.NotEqual(t, "bot_foreign", m.UID, "listMembers 仍应按 creator_uid 过滤掉它")
	}
}

// ---------- kind classification ----------

// TestDirectory_DisabledBotNeverRendersAsHuman 覆盖 queryMembers 的历史缺陷：
// 它的 robot JOIN 不带 status=1 而投影列带，导致调用者自己创建的停用 bot 以
// robot=0 返回、被前端当成真人。目录里停用 bot 整行排除，两个切片都查不到。
func TestDirectory_DisabledBotNeverRendersAsHuman(t *testing.T) {
	srv, _, err := setup(t)
	assert.NoError(t, err)

	spaceID := "dir_space_disabled"
	seedDirectorySpace(t, spaceID, testutil.UID)
	seedDirectoryBot(t, spaceID, "bot_disabled", "Disabled Bot", testutil.UID, 0)

	for _, listType := range []string{directoryTypeBot, directoryTypeHuman, directoryTypeAll} {
		resp := decodeDirectory(t, getDirectory(t, srv, testCtx, spaceID, directoryQuery(listType, 0, 0)))
		assert.NotContains(t, directoryUIDs(resp), "bot_disabled",
			"停用 bot 不应出现在 type=%s", listType)
	}
}

// TestDirectory_ExcludesBotfather 内置 bot 管理账号在任何切片下都不出现。
func TestDirectory_ExcludesBotfather(t *testing.T) {
	srv, _, err := setup(t)
	assert.NoError(t, err)

	spaceID := "dir_space_botfather"
	seedDirectorySpace(t, spaceID, testutil.UID)
	seedDirectoryBot(t, spaceID, directoryExcludedUID, "BotFather", testutil.UID, 1)

	for _, listType := range []string{directoryTypeAll, directoryTypeBot, directoryTypeHuman} {
		resp := decodeDirectory(t, getDirectory(t, srv, testCtx, spaceID, directoryQuery(listType, 0, 0)))
		assert.NotContains(t, directoryUIDs(resp), directoryExcludedUID,
			"botfather 不应出现在 type=%s", listType)
	}
}

// ---------- type partitioning ----------

// TestDirectory_TypePartitionsExactly all 必须恰好是 human ∪ bot：
// 总数相加相等，且 uid 集合相等。任何一边多出或漏掉一类账号都会被抓到。
func TestDirectory_TypePartitionsExactly(t *testing.T) {
	srv, _, err := setup(t)
	assert.NoError(t, err)

	spaceID := "dir_space_partition"
	seedDirectorySpace(t, spaceID, testutil.UID)
	for i := 0; i < 3; i++ {
		seedDirectoryHuman(t, spaceID, fmt.Sprintf("dir_human_%d", i), fmt.Sprintf("Human %d", i), 0)
	}
	for i := 0; i < 4; i++ {
		seedDirectoryBot(t, spaceID, fmt.Sprintf("dir_bot_%d", i), fmt.Sprintf("Bot %d", i), "other", 1)
	}
	// 停用 bot 不属于任何一类，参与其中会让等式两边同时失衡。
	seedDirectoryBot(t, spaceID, "dir_bot_off", "Bot Off", "other", 0)

	all := decodeDirectory(t, getDirectory(t, srv, testCtx, spaceID, directoryQuery(directoryTypeAll, 0, 100)))
	humans := decodeDirectory(t, getDirectory(t, srv, testCtx, spaceID, directoryQuery(directoryTypeHuman, 0, 100)))
	bots := decodeDirectory(t, getDirectory(t, srv, testCtx, spaceID, directoryQuery(directoryTypeBot, 0, 100)))

	assert.Equal(t, all.Total, humans.Total+bots.Total, "total(all) 必须等于 total(human)+total(bot)")
	assert.ElementsMatch(t, directoryUIDs(all), append(directoryUIDs(humans), directoryUIDs(bots)...))

	// kind 与切片自洽。
	for _, item := range humans.Items {
		assert.Equal(t, directoryKindHuman, item.Kind)
		assert.Nil(t, item.Bot, "human 行不应带 bot 字段")
	}
	for _, item := range bots.Items {
		assert.Equal(t, directoryKindBot, item.Kind)
		assert.NotNil(t, item.Bot, "bot 行必须带 bot 字段")
	}
}

// ---------- pagination ----------

// TestDirectory_StablePagingWithTiedSortKeys role + created_at 并非唯一：批量导入
// 的成员会共享同一组值。缺少 uid 这个唯一末位排序键时，LIMIT/OFFSET 逐页翻会漏行
// 或重复行。这里把三个成员的 created_at 强制设成同一时刻，再以 limit=1 走完全程。
func TestDirectory_StablePagingWithTiedSortKeys(t *testing.T) {
	srv, _, err := setup(t)
	assert.NoError(t, err)

	spaceID := "dir_space_paging"
	seedDirectorySpace(t, spaceID, testutil.UID)
	for i := 0; i < 3; i++ {
		seedDirectoryHuman(t, spaceID, fmt.Sprintf("dir_tie_%d", i), fmt.Sprintf("Tie %d", i), 0)
	}
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE space_member SET created_at = '2026-01-01 00:00:00' WHERE space_id = ?", spaceID,
	).Exec()
	assert.NoError(t, err)

	first := decodeDirectory(t, getDirectory(t, srv, testCtx, spaceID, directoryQuery(directoryTypeAll, 1, 1)))
	total := first.Total
	assert.Equal(t, 4, total, "owner + 3 名并列成员")

	seen := map[string]int{}
	for page := 1; page <= total; page++ {
		resp := decodeDirectory(t, getDirectory(t, srv, testCtx, spaceID, directoryQuery(directoryTypeAll, page, 1)))
		assert.Len(t, resp.Items, 1, "第 %d 页应恰好 1 行", page)
		for _, item := range resp.Items {
			seen[item.UID]++
		}
	}
	assert.Len(t, seen, total, "逐页走完应覆盖全部成员，不漏不重")
	for uid, count := range seen {
		assert.Equal(t, 1, count, "%s 出现了 %d 次", uid, count)
	}
}

// TestDirectory_OneRowPerUID 三个 LEFT JOIN 都命中唯一键，一个 uid 恒对应一行。
// 这里显式钉住，避免将来加 JOIN 时悄悄扇出。
func TestDirectory_OneRowPerUID(t *testing.T) {
	srv, _, err := setup(t)
	assert.NoError(t, err)

	spaceID := "dir_space_unique"
	seedDirectorySpace(t, spaceID, testutil.UID)
	seedDirectoryHuman(t, spaceID, "dir_uniq_human", "Uniq Human", 0)
	seedDirectoryBot(t, spaceID, "dir_uniq_bot", "Uniq Bot", "other", 1)
	// 实名记录与好友关系都存在，是最容易扇出的组合。
	_, err = testCtx.DB().InsertBySql(
		"INSERT INTO user_verification (user_id, real_name, source, source_sub, verified_at) VALUES (?,?,?,?,NOW())",
		"dir_uniq_human", "Real Name", "cas", "sub",
	).Exec()
	assert.NoError(t, err)

	resp := decodeDirectory(t, getDirectory(t, srv, testCtx, spaceID, directoryQuery(directoryTypeAll, 0, 100)))
	seen := map[string]int{}
	for _, item := range resp.Items {
		seen[item.UID]++
	}
	for uid, count := range seen {
		assert.Equal(t, 1, count, "%s 出现了 %d 次", uid, count)
	}
	assert.Equal(t, resp.Total, len(resp.Items))
}

// ---------- params ----------

func TestDirectory_ParamNormalization(t *testing.T) {
	srv, _, err := setup(t)
	assert.NoError(t, err)

	spaceID := "dir_space_params"
	seedDirectorySpace(t, spaceID, testutil.UID)

	t.Run("page<=0 归 1，limit<=0 归默认值", func(t *testing.T) {
		q := url.Values{}
		q.Set("page", "0")
		q.Set("limit", "0")
		resp := decodeDirectory(t, getDirectory(t, srv, testCtx, spaceID, q))
		assert.Equal(t, 1, resp.Page)
		assert.Equal(t, directoryDefaultLimit, resp.Limit)
	})

	t.Run("limit 超上限被截断", func(t *testing.T) {
		q := url.Values{}
		q.Set("limit", fmt.Sprintf("%d", directoryMaxLimit+1))
		resp := decodeDirectory(t, getDirectory(t, srv, testCtx, spaceID, q))
		assert.Equal(t, directoryMaxLimit, resp.Limit)
	})

	t.Run("type 非法直接报错而不是静默回落", func(t *testing.T) {
		q := url.Values{}
		q.Set("type", "robots")
		w := getDirectory(t, srv, testCtx, spaceID, q)
		resp := decodeDirectory(t, w)
		assert.Empty(t, resp.Items)
		assert.Contains(t, w.Body.String(), "err.server.space.request_invalid", w.Body.String())
	})

	t.Run("type 缺省等价于 all", func(t *testing.T) {
		seedDirectoryBot(t, spaceID, "dir_default_bot", "Default Bot", "other", 1)
		omitted := decodeDirectory(t, getDirectory(t, srv, testCtx, spaceID, url.Values{}))
		explicit := decodeDirectory(t, getDirectory(t, srv, testCtx, spaceID, directoryQuery(directoryTypeAll, 0, 0)))
		assert.Equal(t, explicit.Total, omitted.Total)
		assert.ElementsMatch(t, directoryUIDs(explicit), directoryUIDs(omitted))
	})
}

// ---------- payload shape ----------

// TestDirectory_RowStaysLean bot 行只带 relation。space_bots 的 description /
// creator_uid / creator_name / bot_commands / auto_approve 一个都不该出现——
// 断言直接打在序列化后的 JSON 上，这样「顺手加个字段」会立刻失败。
func TestDirectory_RowStaysLean(t *testing.T) {
	srv, _, err := setup(t)
	assert.NoError(t, err)

	spaceID := "dir_space_lean"
	seedDirectorySpace(t, spaceID, testutil.UID)
	seedDirectoryBot(t, spaceID, "dir_lean_bot", "Lean Bot", "other", 1)

	w := getDirectory(t, srv, testCtx, spaceID, directoryQuery(directoryTypeBot, 0, 0))
	body := w.Body.String()
	for _, banned := range []string{"description", "creator_uid", "creator_name", "bot_commands", "auto_approve"} {
		assert.NotContains(t, body, banned, "目录行不应携带 %s", banned)
	}

	var raw struct {
		Items []struct {
			Bot map[string]interface{} `json:"bot"`
		} `json:"items"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw), body)
	assert.Len(t, raw.Items, 1)
	assert.Len(t, raw.Items[0].Bot, 1, "bot 对象应只有 relation 一个键: %v", raw.Items[0].Bot)
	assert.Contains(t, raw.Items[0].Bot, "relation")
}

// TestDirectory_NameFallbackInherited 目录复用 #344 的展示名兜底链，
// 不重新实现一套：user.name 空回退 real_name，都空给稳定占位符。
func TestDirectory_NameFallbackInherited(t *testing.T) {
	srv, _, err := setup(t)
	assert.NoError(t, err)

	spaceID := "dir_space_name"
	seedDirectorySpace(t, spaceID, testutil.UID)
	seedDirectoryHuman(t, spaceID, "dir_name_real", "", 0)
	seedDirectoryHuman(t, spaceID, "dir_name_none", "", 0)
	_, err = testCtx.DB().InsertBySql(
		"INSERT INTO user_verification (user_id, real_name, source, source_sub, verified_at) VALUES (?,?,?,?,NOW())",
		"dir_name_real", "实名张三", "cas", "sub",
	).Exec()
	assert.NoError(t, err)

	resp := decodeDirectory(t, getDirectory(t, srv, testCtx, spaceID, directoryQuery(directoryTypeAll, 0, 100)))
	names := map[string]string{}
	for _, item := range resp.Items {
		names[item.UID] = item.Name
	}
	assert.Equal(t, "实名张三", names["dir_name_real"])
	assert.Equal(t, memberDisplayNamePlaceholderPrefix+"dir_name_none", names["dir_name_none"])
	for uid, name := range names {
		assert.NotEmpty(t, name, "%s 的展示名不应为空", uid)
	}
}

// ---------- relation ----------

// TestDirectory_RelationIsViewerScoped relation 是响应里唯一因人而异的字段。
// 三个视角走 DB 层直接验证（HTTP 层只有一个 token 身份可用）。
//
// 同时钉住 pending 的取数：申请记录存在 friend_apply_record，方向是
// uid=被申请方、to_uid=申请人。space_bots 查的 friend_apply 表不存在且方向相反，
// 错误被 `_, _ =` 吞掉，所以它线上永远返回不了 pending。
func TestDirectory_RelationIsViewerScoped(t *testing.T) {
	_, _, err := setup(t)
	assert.NoError(t, err)

	const botUID = "dir_rel_bot"
	const viewerFriend = "dir_rel_viewer_friend"
	const viewerPending = "dir_rel_viewer_pending"
	const viewerNone = "dir_rel_viewer_none"

	_, err = testCtx.DB().InsertBySql(
		"INSERT INTO friend (uid, to_uid, is_deleted) VALUES (?, ?, 0)", viewerFriend, botUID,
	).Exec()
	assert.NoError(t, err)
	_, err = testCtx.DB().InsertBySql(
		"INSERT INTO friend_apply_record (uid, to_uid, status) VALUES (?, ?, 0)", botUID, viewerPending,
	).Exec()
	assert.NoError(t, err)

	for viewer, want := range map[string]string{
		viewerFriend:  directoryRelationAdded,
		viewerPending: directoryRelationPending,
	} {
		relations, err := testSpaceDB.queryBotRelations(viewer, []string{botUID})
		assert.NoError(t, err)
		assert.Equal(t, want, relations[botUID], "viewer %s", viewer)
	}

	relations, err := testSpaceDB.queryBotRelations(viewerNone, []string{botUID})
	assert.NoError(t, err)
	assert.Empty(t, relations[botUID], "无关系时由 handler 兜底为 not_added")

	// 已通过（status=1）/ 已拒绝（status=2）的申请都不算 pending。
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE friend_apply_record SET status = 2 WHERE uid = ? AND to_uid = ?", botUID, viewerPending,
	).Exec()
	assert.NoError(t, err)
	relations, err = testSpaceDB.queryBotRelations(viewerPending, []string{botUID})
	assert.NoError(t, err)
	assert.Empty(t, relations[botUID], "已拒绝的申请不应算作 pending")
}

// TestDirectory_HumanSliceCarriesNoBotPayload type=human 的行不带 bot 字段，
// 且关系态查询在 uid 列表为空时直接短路。
//
// 诚实地说明本用例**没有**覆盖什么：它证明不了 handler 真的跳过了
// queryBotRelations。fixture 里的 friend 行指向一个本就不在 human 切片里的 bot，
// 所以即便关系态查询照跑，输出也一模一样。要真正钉住「跳过」需要在 DB 调用层
// 打桩计数，本包目前没有这个能力。brief 验收里那条「asserted at the DB-call
// level」因此仍未满足——记在这里，而不是让用例名字替它撒谎。
func TestDirectory_HumanSliceCarriesNoBotPayload(t *testing.T) {
	srv, _, err := setup(t)
	assert.NoError(t, err)

	spaceID := "dir_space_skip"
	seedDirectorySpace(t, spaceID, testutil.UID)
	seedDirectoryHuman(t, spaceID, "dir_skip_human", "Skip Human", 0)
	seedDirectoryBot(t, spaceID, "dir_skip_bot", "Skip Bot", "other", 1)
	// 即使这条好友关系存在，type=human 也不该去查它。
	_, err = testCtx.DB().InsertBySql(
		"INSERT INTO friend (uid, to_uid, is_deleted) VALUES (?, ?, 0)", testutil.UID, "dir_skip_bot",
	).Exec()
	assert.NoError(t, err)

	resp := decodeDirectory(t, getDirectory(t, srv, testCtx, spaceID, directoryQuery(directoryTypeHuman, 0, 100)))
	assert.NotEmpty(t, resp.Items)
	for _, item := range resp.Items {
		assert.Equal(t, directoryKindHuman, item.Kind)
		assert.Nil(t, item.Bot)
	}

	relations, err := testSpaceDB.queryBotRelations(testutil.UID, nil)
	assert.NoError(t, err)
	assert.Empty(t, relations)
}

// ---------- frozen legacy endpoint ----------

// TestDirectory_LegacyMembersUnaffectedByIsBot 钉住「已弃用接口禁止改动」这条约束
// 在本次 schema 变更下依然成立。
//
// space_member 新增的 is_bot 列会被 queryMembers 的 `SELECT sm.*` 一并取出。列名
// 刻意避开 robot 就是为了不与它自己的 `CASE ... as robot` 别名在结果集里撞车——
// 一旦撞车，listMembers 返回的 robot 取值会静默改变，而移动端与已发布的 web 构建
// 都在读这个字段。这里用一个同时含真人、启用 bot、停用 bot 的空间做断言。
func TestDirectory_LegacyMembersUnaffectedByIsBot(t *testing.T) {
	_, _, err := setup(t)
	assert.NoError(t, err)

	spaceID := "dir_space_legacy"
	seedDirectorySpace(t, spaceID, testutil.UID)
	seedDirectoryHuman(t, spaceID, "lg_human", "Legacy Human", 0)
	seedDirectoryBot(t, spaceID, "lg_bot_mine", "Mine", testutil.UID, 1)
	seedDirectoryBot(t, spaceID, "lg_bot_off", "Mine Disabled", testutil.UID, 0)
	seedDirectoryBot(t, spaceID, "lg_bot_other", "Someone Else's", "another_creator", 1)

	members, err := testSpaceDB.queryMembers(spaceID, testutil.UID, 1, 100)
	assert.NoError(t, err)

	robotByUID := map[string]int{}
	for _, m := range members {
		robotByUID[m.UID] = m.Robot
	}

	// robot 仍由 queryMembers 自己的 CASE 决定（robot 行存在且 status=1），
	// 不是新列的值。停用 bot 依旧以 robot=0 出现——这正是那个已知缺陷，
	// 冻结意味着它保持原样，而不是被这次改动顺手改掉。
	assert.Equal(t, 0, robotByUID["lg_human"], "真人 robot=0")
	assert.Equal(t, 1, robotByUID["lg_bot_mine"], "自己创建的启用 bot robot=1")
	assert.Equal(t, 0, robotByUID["lg_bot_off"], "自己创建的停用 bot 仍以 robot=0 返回（已知缺陷，冻结）")

	// creator 过滤同样保持原样：别人创建的 bot 依旧不可见。
	_, visible := robotByUID["lg_bot_other"]
	assert.False(t, visible, "别人创建的 bot 仍应被 creator_uid 过滤掉")
}

// TestDirectory_TotalStaysCorrectWhenCountIsElided total 在「本页即全部」时由行数
// 推出、跳过单独的 COUNT 查询。这里钉住三个分支，尤其是最容易写错的边界：
// 取回行数恰好等于 limit 时无法区分「刚好取完」与「还有下一页」，必须真的去数。
func TestDirectory_TotalStaysCorrectWhenCountIsElided(t *testing.T) {
	srv, _, err := setup(t)
	assert.NoError(t, err)

	spaceID := "dir_space_total"
	seedDirectorySpace(t, spaceID, testutil.UID) // owner 一行
	for i := 0; i < 4; i++ {
		seedDirectoryHuman(t, spaceID, fmt.Sprintf("dir_tot_%d", i), fmt.Sprintf("Tot %d", i), 0)
	}
	const want = 5 // owner + 4

	t.Run("limit 大于总数：total 由行数推出", func(t *testing.T) {
		resp := decodeDirectory(t, getDirectory(t, srv, testCtx, spaceID, directoryQuery(directoryTypeAll, 1, 100)))
		assert.Equal(t, want, resp.Total)
		assert.Len(t, resp.Items, want)
	})

	t.Run("limit 恰好等于总数：仍须实际计数", func(t *testing.T) {
		resp := decodeDirectory(t, getDirectory(t, srv, testCtx, spaceID, directoryQuery(directoryTypeAll, 1, want)))
		assert.Equal(t, want, resp.Total, "边界上 total 不能退化成 len(items) 之外的值")
		assert.Len(t, resp.Items, want)
	})

	t.Run("非首页：total 是全量而非本页行数", func(t *testing.T) {
		resp := decodeDirectory(t, getDirectory(t, srv, testCtx, spaceID, directoryQuery(directoryTypeAll, 2, 2)))
		assert.Equal(t, want, resp.Total, "第二页的 total 必须仍是 %d", want)
		assert.Len(t, resp.Items, 2)
	})

	t.Run("越界页：total 保持全量，items 为空", func(t *testing.T) {
		resp := decodeDirectory(t, getDirectory(t, srv, testCtx, spaceID, directoryQuery(directoryTypeAll, 99, 10)))
		assert.Equal(t, want, resp.Total)
		assert.Empty(t, resp.Items)
	})
}
