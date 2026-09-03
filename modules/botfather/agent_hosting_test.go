package botfather

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-server/modules/bot_api"
	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	// app_bot 的迁移建 app_bot 表，TestRegisterAppBotIgnoresAgentRuntimeFields 需要它。
	// 本包既有的 blank import（api_bot_group_test.go / testinit_test.go）是同一形状。
	// 刻意**不**照抄 modules/bot_api/registry_redis_multiinstance_test.go 的裸
	// `CREATE TABLE IF NOT EXISTS app_bot`：不写 gorp_migrations 的裸建表会让下一个
	// 包的 NewTestServer 跑同名迁移时撞 "Table already exists"（modules/message 大量
	// DB 测试被 skip 就是这个根因）。
	_ "github.com/Mininglamp-OSS/octo-server/modules/app_bot"
)

// Agent 自报托管形态（agent_hosting）的落库与读出测试。
//
// **为什么这些测试住在 botfather 而不是 bot_api**（写入路径在
// modules/bot_api/register.go）：robot 表的 agent_* 列由本模块的迁移拥有
// （20260417000001 的三列 + 20260903000001 的两列），而 bot_api 包的测试二进制
// 不 link 本模块的 init，NewTestServer 在那里建出来的 robot 表压根没有这些列
// （实测 `Error 1054: Unknown column 'agent_platform'`）。本包已 blank import
// bot_api（见 api_bot_group_test.go），所以 /v1/bot/register 路由在这里可用，
// 且 schema 完整。
//
// 纯函数（normalizeAgentHosting）与源码守卫留在 bot_api 包，就近于被测代码。

type agentInfoRow struct {
	AgentPlatform   string       `db:"agent_platform"`
	AgentVersion    string       `db:"agent_version"`
	PluginVersion   string       `db:"plugin_version"`
	AgentHosting    string       `db:"agent_hosting"`
	AgentReportedAt dbr.NullTime `db:"agent_reported_at"`
}

func readAgentInfo(t *testing.T, ctx *config.Context, robotID string) agentInfoRow {
	t.Helper()
	var row agentInfoRow
	err := ctx.DB().SelectBySql(
		"SELECT IFNULL(agent_platform,'') AS agent_platform, IFNULL(agent_version,'') AS agent_version, "+
			"IFNULL(plugin_version,'') AS plugin_version, IFNULL(agent_hosting,'') AS agent_hosting, "+
			"agent_reported_at FROM robot WHERE robot_id=?", robotID,
	).LoadOne(&row)
	require.NoError(t, err)
	return row
}

// botRegister 用 bf_ token 调 POST /v1/bot/register。
func botRegister(t *testing.T, h http.Handler, botToken string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, userAPIRequest(t, http.MethodPost, "/v1/bot/register", botToken, body))
	return w
}

// setupAgentHostingBot 造一个可 register 的 User Bot，返回 (robotID, bf_ token)。
func setupAgentHostingBot(t *testing.T, ctx *config.Context, tag string) (string, string) {
	t.Helper()
	robotID := tag + "_" + util.GenerUUID()[:6]
	insertTestUser(t, ctx, "owner_"+robotID, "owner "+robotID)
	botToken := insertTestBot(t, ctx, robotID, "owner_"+robotID)
	insertTestUser(t, ctx, robotID, robotID)
	return robotID, botToken
}

// TestRegisterStoresAgentHosting —— 正常上报落库，且盖上 reported_at。
func TestRegisterStoresAgentHosting(t *testing.T) {
	h, ctx := newUserAPITestServer(t)
	resetUIDRateLimit(t, ctx)
	robotID, botToken := setupAgentHostingBot(t, ctx, "hostok")

	w := botRegister(t, h, botToken, map[string]interface{}{
		"agent_platform": "OpenClaw",
		"agent_version":  "0.3.1",
		"plugin_version": "1.2.0",
		"agent_hosting":  "self_hosted",
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	row := readAgentInfo(t, ctx, robotID)
	assert.Equal(t, bot_api.AgentHostingSelfHosted, row.AgentHosting)
	assert.Equal(t, "OpenClaw", row.AgentPlatform)
	assert.True(t, row.AgentReportedAt.Valid,
		"上报后 agent_reported_at 必须非 NULL —— 缺了它 agent_hosting 就是个无从判断新鲜度的裸值")
}

// TestRegisterMalformedHostingDoesNotFailRegister —— 形状非法的上报绝不阻断 register。
//
// register 是 Bot 掉线后自愈的唯一通道（#696 的二次事故正是 register 被连带拒绝，
// bot 再也起不来）。一个纯观测字段的形状校验不能把它变成失败路径：值降级为
// 「未上报」，请求照常 200。
//
// 注意用的是**注入形状**的输入而不是 "cloud" —— 取值现在是开放的，`cloud` 会合法
// 通过（见 bot_api.TestNormalizeAgentHosting 里记的已知代价）。这里要验的是
// caller-controlled 字节被挡在库外，不是取值不在某个集合里。
func TestRegisterMalformedHostingDoesNotFailRegister(t *testing.T) {
	h, ctx := newUserAPITestServer(t)
	resetUIDRateLimit(t, ctx)
	robotID, botToken := setupAgentHostingBot(t, ctx, "hostbad")

	w := botRegister(t, h, botToken, map[string]interface{}{
		"agent_platform": "OpenClaw",
		"agent_hosting":  `<script>alert(1)</script>`,
	})
	require.Equal(t, http.StatusOK, w.Code, "形状非法的取值不得让 register 失败")

	row := readAgentInfo(t, ctx, robotID)
	assert.Equal(t, "", row.AgentHosting, "形状非法的值必须落空串，不得原样入库")
	assert.Equal(t, "OpenClaw", row.AgentPlatform, "同一请求里的合法字段仍应落库")
}

// TestRegisterThirdPartyHostingRoundTrips —— 第三方托管方无需改服务端即可自报，
// 且值原样落库、原样出现在 owner 面。
//
// 这是「取值开放」这个决策的端到端验证：白名单实现下第三方 slug 会被落成空串，
// 需要改服务端代码 + 发版才能支持。
func TestRegisterThirdPartyHostingRoundTrips(t *testing.T) {
	h, ctx := newUserAPITestServer(t)
	resetUIDRateLimit(t, ctx)
	robotID, botToken := setupAgentHostingBot(t, ctx, "host3rd")

	const vendor = "vendor_hosted"
	require.Equal(t, http.StatusOK, botRegister(t, h, botToken, map[string]interface{}{
		"agent_hosting": vendor,
	}).Code)
	assert.Equal(t, vendor, readAgentInfo(t, ctx, robotID).AgentHosting,
		"第三方 <vendor>_hosted 必须原样落库，不需要服务端改代码")

	// owner 面原样带出（前端展示 slug，不做映射表 —— 新托管方前端也不用发版）。
	key := mintUserAPIKey(t, ctx, "owner_"+robotID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, userAPIRequest(t, http.MethodGet, "/v1/user/bots", key, nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"agent_hosting":"`+vendor+`"`)
}

// TestRegisterHostingPointerSemantics —— 指针语义：字段缺席则保留，显式空串则清空。
//
// 与同组三个字符串字段的「空值保留旧值」merge 刻意不同：自运维→平台托管切换时，
// 新 runtime 漏报会留下陈旧的 self_hosted，而这个字段的陈旧值比空值更有害。
func TestRegisterHostingPointerSemantics(t *testing.T) {
	h, ctx := newUserAPITestServer(t)
	resetUIDRateLimit(t, ctx)
	robotID, botToken := setupAgentHostingBot(t, ctx, "hostptr")

	require.Equal(t, http.StatusOK, botRegister(t, h, botToken, map[string]interface{}{
		"agent_hosting": "octo_hosted",
	}).Code)
	require.Equal(t, bot_api.AgentHostingOctoHosted, readAgentInfo(t, ctx, robotID).AgentHosting)

	// 字段缺席（旧客户端 / 只报版本）→ 保留已存值。
	require.Equal(t, http.StatusOK, botRegister(t, h, botToken, map[string]interface{}{
		"agent_version": "0.9.9",
	}).Code)
	row := readAgentInfo(t, ctx, robotID)
	assert.Equal(t, bot_api.AgentHostingOctoHosted, row.AgentHosting, "字段缺席时必须保留已存值")
	assert.Equal(t, "0.9.9", row.AgentVersion)

	// 显式空串 → 清空。与「缺席」可区分，这正是用指针的理由。
	require.Equal(t, http.StatusOK, botRegister(t, h, botToken, map[string]interface{}{
		"agent_hosting": "",
	}).Code)
	assert.Equal(t, "", readAgentInfo(t, ctx, robotID).AgentHosting, "显式空串必须清空")
}

// TestRegisterRefreshesReportedAtWhenValuesUnchanged —— reported_at 的语义是
// 「最近一次收到上报」，不是「值变更时间」，所以值没变也必须前进。
//
// 手动把时间戳推到过去再上报，避开 TIMESTAMP 的秒级精度（两次快速调用可能落在
// 同一秒，直接比较会假阴性）。
func TestRegisterRefreshesReportedAtWhenValuesUnchanged(t *testing.T) {
	h, ctx := newUserAPITestServer(t)
	resetUIDRateLimit(t, ctx)
	robotID, botToken := setupAgentHostingBot(t, ctx, "hostts")

	body := map[string]interface{}{"agent_platform": "OpenClaw", "agent_hosting": "self_hosted"}
	require.Equal(t, http.StatusOK, botRegister(t, h, botToken, body).Code)

	_, err := ctx.DB().UpdateBySql(
		"UPDATE robot SET agent_reported_at='2020-01-01 00:00:00' WHERE robot_id=?", robotID,
	).Exec()
	require.NoError(t, err)
	stale := readAgentInfo(t, ctx, robotID).AgentReportedAt
	require.True(t, stale.Valid)

	// 完全相同的 body 再报一次。
	require.Equal(t, http.StatusOK, botRegister(t, h, botToken, body).Code)

	fresh := readAgentInfo(t, ctx, robotID).AgentReportedAt
	require.True(t, fresh.Valid)
	assert.True(t, fresh.Time.After(stale.Time),
		"值未变化时 agent_reported_at 仍须刷新，否则它回答的是「值何时改变」而非「何时收到上报」")
}

// TestRegisterEmptyBodyLeavesAgentInfoUntouched —— 旧客户端（空 body / 无 body）
// 的 register 必须完全不碰 agent_* 列。
//
// 这条守的是「无条件 UPDATE」那个改动的边界：外层 if 仍要求至少提供一个 agent 字段，
// 否则每一次重连 register 都会写一次库，并给一行从未上报过的 Bot 盖上 reported_at ——
// 那会让「收到过上报」这个语义失真，也是纯写放大。
func TestRegisterEmptyBodyLeavesAgentInfoUntouched(t *testing.T) {
	h, ctx := newUserAPITestServer(t)
	resetUIDRateLimit(t, ctx)
	robotID, botToken := setupAgentHostingBot(t, ctx, "hostnone")

	// 空 JSON 对象。
	require.Equal(t, http.StatusOK, botRegister(t, h, botToken, map[string]interface{}{}).Code)
	row := readAgentInfo(t, ctx, robotID)
	assert.Equal(t, "", row.AgentHosting)
	assert.False(t, row.AgentReportedAt.Valid,
		"没上报任何 agent 字段时不得盖 reported_at —— 否则「收到过上报」的语义失真")

	// 完全没有 body（最老的客户端形态）。
	w := httptest.NewRecorder()
	h.ServeHTTP(w, userAPIRequest(t, http.MethodPost, "/v1/bot/register", botToken, nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.False(t, readAgentInfo(t, ctx, robotID).AgentReportedAt.Valid,
		"无 body 的 register 同样不得写 agent_* 列")
}

// TestRegisterVersionOnlyReportKeepsHostingButAdvancesTimestamp —— 只报版本时
// agent_hosting 列**不被写**，而 reported_at 仍然前进。
//
// 两个断言合起来才有意义：只看 hosting 不变，回写旧值的实现也能过；配上
// reported_at 前进就能确认 UPDATE 确实执行了，只是没有把 agent_hosting 列带进
// SetMap —— 那正是避免并发 register 丢更新的写法（见 updateRobotAgentInfo）。
func TestRegisterVersionOnlyReportKeepsHostingButAdvancesTimestamp(t *testing.T) {
	h, ctx := newUserAPITestServer(t)
	resetUIDRateLimit(t, ctx)
	robotID, botToken := setupAgentHostingBot(t, ctx, "hostkeep")

	require.Equal(t, http.StatusOK, botRegister(t, h, botToken, map[string]interface{}{
		"agent_hosting": "self_hosted",
	}).Code)
	_, err := ctx.DB().UpdateBySql(
		"UPDATE robot SET agent_reported_at='2020-01-01 00:00:00' WHERE robot_id=?", robotID,
	).Exec()
	require.NoError(t, err)
	stale := readAgentInfo(t, ctx, robotID).AgentReportedAt
	require.True(t, stale.Valid)

	// 只报版本，不带 agent_hosting。
	require.Equal(t, http.StatusOK, botRegister(t, h, botToken, map[string]interface{}{
		"plugin_version": "2.0.0",
	}).Code)

	row := readAgentInfo(t, ctx, robotID)
	assert.Equal(t, bot_api.AgentHostingSelfHosted, row.AgentHosting, "缺席的字段不得被清空")
	assert.Equal(t, "2.0.0", row.PluginVersion)
	assert.True(t, row.AgentReportedAt.Time.After(stale.Time),
		"本次上报确实写了库（reported_at 前进），只是没碰 agent_hosting 列")
}

// TestRegisterAppBotIgnoresAgentRuntimeFields —— App Bot 的上报不落库（brief 方案 A）。
func TestRegisterAppBotIgnoresAgentRuntimeFields(t *testing.T) {
	h, ctx := newUserAPITestServer(t)
	resetUIDRateLimit(t, ctx)

	uid := "app_hosting_" + util.GenerUUID()[:6] + "_bot"
	token := "app_" + util.GenerUUID()[:16]
	// space_id 写空串而非 NULL：app_bot.space_id 允许 NULL，但 bot_api 的
	// appBotModel.SpaceID 是非 nullable 的 string，NULL 行会让 queryAppBotByToken
	// 的 Select("*") 扫描失败并把鉴权变成 500。生产写入路径（modules/app_bot/db.go）
	// 走的是同一个 string 字段，落的就是空串，所以这里对齐生产。
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO app_bot (id,uid,display_name,scope,space_id,status,token,created_by) "+
			"VALUES (?,?,?,'platform','',1,?,'admin')",
		util.GenerUUID(), uid, "hosting app bot", token,
	).Exec()
	require.NoError(t, err)
	insertTestUser(t, ctx, uid, uid)

	w := botRegister(t, h, token, map[string]interface{}{
		"agent_platform": "OpenClaw",
		"agent_hosting":  "self_hosted",
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// App Bot 的 uid 不属于 robot 表 —— 上报绝不能在那里凭空造行。
	var robotRows int
	require.NoError(t, ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM robot WHERE robot_id=?", uid).LoadOne(&robotRows))
	assert.Equal(t, 0, robotRows, "App Bot 的上报绝不能写进 robot 表")
}

// TestListUserBotsExposesAgentHosting —— owner 读出面。
//
// 未上报时的形状也钉住：agent_hosting 省略（与同组 agent_platform 一致），
// agent_reported_at 显式 null（与 bound_at 一致）。
func TestListUserBotsExposesAgentHosting(t *testing.T) {
	h, ctx := newUserAPITestServer(t)
	resetUIDRateLimit(t, ctx)

	uid := "owner_hostlist_" + util.GenerUUID()[:6]
	insertTestUser(t, ctx, uid, uid)
	reported := "bot_hostlist_" + util.GenerUUID()[:6]
	insertTestBot(t, ctx, reported, uid)
	insertTestUser(t, ctx, reported, reported)
	silent := "bot_hostmute_" + util.GenerUUID()[:6]
	insertTestBot(t, ctx, silent, uid)
	insertTestUser(t, ctx, silent, silent)

	_, err := ctx.DB().UpdateBySql(
		"UPDATE robot SET agent_hosting='octo_hosted', agent_reported_at=NOW() WHERE robot_id=?", reported,
	).Exec()
	require.NoError(t, err)

	key := mintUserAPIKey(t, ctx, uid)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, userAPIRequest(t, http.MethodGet, "/v1/user/bots", key, nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	body := w.Body.String()
	assert.Contains(t, body, `"agent_hosting":"octo_hosted"`, "已上报的 Bot 必须带出托管形态")
	// 未上报的那个：字段省略而不是空串（omitempty，与 agent_platform 同组行为）。
	assert.NotContains(t, body, `"agent_hosting":""`,
		"未上报时 agent_hosting 应被 omitempty 省略，而不是下发空串")
	assert.Contains(t, body, `"agent_reported_at":null`,
		"未上报时 agent_reported_at 必须显式 null（与 bound_at 同口径），而不是省略字段")
}

// TestRegisterOverlongPlatformBlocksTheWholeAgentUpdate —— **记录已知限制**，
// 不是在庆祝它。
//
// 三个既有字符串字段（agent_platform / agent_version / plugin_version）没有任何
// 长度校验，列宽是 VARCHAR(50)，而测试库和生产都开着 STRICT_TRANS_TABLES
// （实测 `INSERT REPEAT('x',200)` into VARCHAR(50) → `1406 Data too long`）。
// agent_* 全组共用**一条** UPDATE，所以一个超长的 agent_platform 会让整条语句失败，
// 连带把同一请求里合法的 agent_hosting 也挡在库外 —— 而 register 仍返回 200，
// 客户端看不出来（失败只进日志，带 1406）。
//
// 为什么本任务不修：修法要么给三个既有字段加「超长则忽略该字段」的降级（改的是
// 既有字段行为，超出本任务范围），要么把 UPDATE 拆成两条（放弃原子性换解耦）。
// 两者都该单独决策。改动前后这个 UPDATE 都会失败（旧代码里超长值同样存不进去，
// 且因为值永远不等于库里的值，每次 register 都会重试并再次失败），所以不是本任务
// 引入的回归 —— 但新字段的可写性现在**依赖旧字段的卫生**，这一点必须被记录。
//
// 这条测试是可执行的文档：谁哪天加了字段级降级，它会红，提醒同步更新 brief 的
// 已知限制段。
func TestRegisterOverlongPlatformBlocksTheWholeAgentUpdate(t *testing.T) {
	h, ctx := newUserAPITestServer(t)
	resetUIDRateLimit(t, ctx)
	robotID, botToken := setupAgentHostingBot(t, ctx, "hostlong")

	require.Equal(t, http.StatusOK, botRegister(t, h, botToken, map[string]interface{}{
		"agent_platform": strings.Repeat("x", 200), // 列宽 50
		"agent_hosting":  "vendor_hosted",          // 本身完全合法
	}).Code, "UPDATE 失败只记日志，register 仍须返回 200")

	row := readAgentInfo(t, ctx, robotID)
	assert.Equal(t, "", row.AgentPlatform, "超长值本就写不进 VARCHAR(50)")
	assert.Equal(t, "", row.AgentHosting,
		"已知限制：同一条 UPDATE 里的合法 agent_hosting 被超长的 agent_platform 连带挡住")
	assert.False(t, row.AgentReportedAt.Valid, "整条 UPDATE 失败，时间戳同样没落下")
}

// TestAgentHostingMigrationIsSingleAtomicAlter —— 两列必须在同一条 ALTER 里。
//
// 本仓迁移原则是朴素 DDL + 靠 gorp_migrations 记账，不在每条迁移里堆幂等魔法。
// 该原则成立的前提是这条迁移**原子**：单条 ALTER 加两列要么全成要么全不成，
// 不留「一列已加一列没加」的中间态（#239 的半应用态出在多语句迁移上，
// 20260417000001 的 agent_* 三列同样是三条独立 ALTER、同样可半应用）。
// 拆成两条 ALTER 又不加守卫，是最坏的组合。
func TestAgentHostingMigrationIsSingleAtomicAlter(t *testing.T) {
	raw, err := os.ReadFile("sql/20260903000001_botfather_agent_hosting.sql")
	require.NoError(t, err)
	src := string(raw)

	up := src[strings.Index(src, "-- +migrate Up"):strings.Index(src, "-- +migrate Down")]
	assert.Equal(t, 1, strings.Count(up, "ALTER TABLE"),
		"Up 段必须只有一条 ALTER TABLE（两列同批加，保证原子）")
	assert.Contains(t, up, "agent_hosting")
	assert.Contains(t, up, "agent_reported_at")
	assert.NotContains(t, up, "INFORMATION_SCHEMA",
		"朴素 DDL 原则：不要在迁移里堆存在性守卫（单条 ALTER 本身已原子）")
	assert.NotContains(t, up, "CREATE PROCEDURE",
		"朴素 DDL 原则：不要用存储过程包幂等逻辑")
	assert.Contains(t, up, "不可用于鉴权",
		"列 COMMENT 必须写明自报值不可用于鉴权")
}
