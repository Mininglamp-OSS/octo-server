package botfather

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-server/modules/bot_api"
	"github.com/go-redis/redis"
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
	AgentPlatform          string       `db:"agent_platform"`
	AgentVersion           string       `db:"agent_version"`
	PluginVersion          string       `db:"plugin_version"`
	AgentHosting           string       `db:"agent_hosting"`
	AgentReportedHostingAt dbr.NullTime `db:"agent_reported_hosting_at"`
}

func readAgentInfo(t *testing.T, ctx *config.Context, robotID string) agentInfoRow {
	t.Helper()
	var row agentInfoRow
	err := ctx.DB().SelectBySql(
		"SELECT IFNULL(agent_platform,'') AS agent_platform, IFNULL(agent_version,'') AS agent_version, "+
			"IFNULL(plugin_version,'') AS plugin_version, IFNULL(agent_hosting,'') AS agent_hosting, "+
			"agent_reported_hosting_at FROM robot WHERE robot_id=?", robotID,
	).LoadOne(&row)
	require.NoError(t, err)
	return row
}

// resetRegisterRateLimit 清掉 /v1/bot/register 的 per-IP strict 桶。
//
// 必须做，且必须在**每次** register 之前：该路由挂的是
// StrictIPRateLimintMiddleware("bot_register")（modules/bot_api/bot_api.go），
// 桶按 **IP** 计，而整包跑时所有测试共享同一个 httptest 客户端 IP。本文件的测试是
// 全仓 register 调用最密集的一批（单条测试里连发 4 次），单跑时桶够用，整包跑时
// 前面积累的调用会把它打满，于是后面的测试收到 429 —— 表现为「单跑绿、整包红，
// 且每次红的还不是同一条」。桶存在 Redis 里，CleanAllTables 不清它。
func resetRegisterRateLimit(t *testing.T, ctx *config.Context) {
	t.Helper()
	rds := redis.NewClient(&redis.Options{
		Addr:     ctx.GetConfig().DB.RedisAddr,
		Password: ctx.GetConfig().DB.RedisPass,
	})
	defer rds.Close()
	// 精确 DEL，不用 KEYS 扫描（round 5 🔵）：KEYS 在每次 register 前全库扫一遍，
	// 而且吞掉错误就等于"没清成功也不知道"。StrictIPRateLimitMiddleware 的 key 形状是
	// `ratelimit:strict:{tag}:{ip}`，httptest 的客户端 IP 固定是 192.0.2.1
	// （net/http/httptest.NewRequest 的 RemoteAddr），所以键是可枚举的。
	//
	// 仍保留一次带 tag 的兜底扫描：key 形状是 lib 侧的实现细节，万一它变了，
	// 精确 DEL 会静默失效 —— 那时兜底能让测试继续可用，而不是变成难查的 429。
	const ip = "192.0.2.1"
	exact := []string{
		"ratelimit:strict:bot_register:" + ip,
		"ratelimit:bot:register:" + ip,
	}
	require.NoError(t, rds.Del(exact...).Err(), "清 per-IP 限流桶失败")
	if keys, err := rds.Keys("*bot_register*").Result(); err == nil && len(keys) > 0 {
		_ = rds.Del(keys...).Err()
	}
}

// botRegister 用 bf_ token 调 POST /v1/bot/register。
//
// 每次调用前清 per-IP 桶 —— 见 resetRegisterRateLimit。放在这里而不是各测试的 setup，
// 是因为限流按调用**次数**累积，setup 时清一次不够：一条测试内连发多次就会自己把桶打满。
func botRegister(t *testing.T, ctx *config.Context, h http.Handler, botToken string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	resetRegisterRateLimit(t, ctx)
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

// TestRegisterStoresAgentHosting —— 正常上报落库，且盖上 hosting 上报时间戳。
func TestRegisterStoresAgentHosting(t *testing.T) {
	h, ctx := newUserAPITestServer(t)
	resetUIDRateLimit(t, ctx)
	robotID, botToken := setupAgentHostingBot(t, ctx, "hostok")

	w := botRegister(t, ctx, h, botToken, map[string]interface{}{
		"agent_platform": "OpenClaw",
		"agent_version":  "0.3.1",
		"plugin_version": "1.2.0",
		"agent_hosting":  "self_hosted",
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	row := readAgentInfo(t, ctx, robotID)
	assert.Equal(t, bot_api.AgentHostingSelfHosted, row.AgentHosting)
	assert.Equal(t, "OpenClaw", row.AgentPlatform)
	assert.True(t, row.AgentReportedHostingAt.Valid,
		"上报 hosting 后时间戳必须非 NULL —— 缺了它 agent_hosting 就是个无从判断新鲜度的裸值")
}

// TestRegisterMalformedHostingPreservesStoredValue —— 形状非法的上报既不阻断
// register，也**不清空**已存的合法值。
//
// 两件事一起断言，因为初版只做到了前一半：`hosting = &normalized` 写在合法性分支
// 外面，于是被拒的值以空串落进 SetMap 覆盖已存值。PR 描述说的是「degrades to
// 'not reported'」（读作保持不动），字段注释的规则是「present overwrites」
// （实际清空）—— 两种读法在同一个 PR 里互相矛盾。现在定为**保持不变**：
// 触发场景不需要恶意，`self-hosted`（带连字符，正是本命名引用的 GitHub Actions
// 写法）就会被拒，一次客户端拼错会把全量 bot 的这一列刷空，只留一行日志。
// 撤回仍然可做，但要显式报 `none`（那是格式良好的上报，不是被拒的上报）——
// 见 TestRegisterHostingNoneClearsTheShape。
//
// 起点是一个**已有合法值**的 bot —— 初版测试从「从未上报」起步，区分不了
// 「存了空串」和「本来就空」，所以完全覆盖不到这个场景。
func TestRegisterMalformedHostingPreservesStoredValue(t *testing.T) {
	h, ctx := newUserAPITestServer(t)
	resetUIDRateLimit(t, ctx)
	robotID, botToken := setupAgentHostingBot(t, ctx, "hostbad")

	require.Equal(t, http.StatusOK, botRegister(t, ctx, h, botToken, map[string]interface{}{
		"agent_hosting": "octo_hosted",
	}).Code)
	require.Equal(t, bot_api.AgentHostingOctoHosted, readAgentInfo(t, ctx, robotID).AgentHosting)

	for _, bad := range []string{
		`<script>alert(1)</script>`, // caller-controlled 字节
		"self-hosted",               // 现实中最可能的拼错：连字符
		"K_hosted",                  // U+212A KELVIN SIGN，ToLower 会折成 'k'
	} {
		w := botRegister(t, ctx, h, botToken, map[string]interface{}{
			"agent_platform": "OpenClaw",
			"agent_hosting":  bad,
		})
		require.Equal(t, http.StatusOK, w.Code, "形状非法的取值不得让 register 失败: %q", bad)

		row := readAgentInfo(t, ctx, robotID)
		assert.Equal(t, bot_api.AgentHostingOctoHosted, row.AgentHosting,
			"被拒的值 %q 必须保持已存值不变，而不是清成空串", bad)
		assert.Equal(t, "OpenClaw", row.AgentPlatform, "同一请求里的合法字段仍应落库")
	}
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
	require.Equal(t, http.StatusOK, botRegister(t, ctx, h, botToken, map[string]interface{}{
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

// TestRegisterHostingPointerSemantics —— 指针语义的可观察结果：字段缺席则保留。
//
// 四个字段现在都是指针（PR #837 round 1 后统一），所以「缺席 vs 报了空」对每个字段
// 都可区分。清空 hosting 用保留 slug（见 AgentHostingNone），不用空串 —— 理由见
// TestRegisterHostingNoneClearsTheShape。
//
// 同样注意：本测试证明"缺席时值保留"，**不**区分稀疏写入与 read-merge-write。
func TestRegisterHostingPointerSemantics(t *testing.T) {
	h, ctx := newUserAPITestServer(t)
	resetUIDRateLimit(t, ctx)
	robotID, botToken := setupAgentHostingBot(t, ctx, "hostptr")

	require.Equal(t, http.StatusOK, botRegister(t, ctx, h, botToken, map[string]interface{}{
		"agent_hosting": "octo_hosted",
	}).Code)
	require.Equal(t, bot_api.AgentHostingOctoHosted, readAgentInfo(t, ctx, robotID).AgentHosting)

	// 字段缺席（旧客户端 / 只报版本）→ 保留已存值。
	require.Equal(t, http.StatusOK, botRegister(t, ctx, h, botToken, map[string]interface{}{
		"agent_version": "0.9.9",
	}).Code)
	row := readAgentInfo(t, ctx, robotID)
	assert.Equal(t, bot_api.AgentHostingOctoHosted, row.AgentHosting, "字段缺席时必须保留已存值")
	assert.Equal(t, "0.9.9", row.AgentVersion)

	// 显式空串 → **保持不变**（"" 对四个字段一律是「未上报」）。撤回走 none，
	// 见 TestRegisterHostingNoneClearsTheShape。
	require.Equal(t, http.StatusOK, botRegister(t, ctx, h, botToken, map[string]interface{}{
		"agent_hosting": "",
	}).Code)
	assert.Equal(t, bot_api.AgentHostingOctoHosted, readAgentInfo(t, ctx, robotID).AgentHosting,
		"空串是「未上报」而非「清空」—— 与三个 legacy 字段同口径（round 4 P2-4）")
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
	require.Equal(t, http.StatusOK, botRegister(t, ctx, h, botToken, body).Code)

	_, err := ctx.DB().UpdateBySql(
		"UPDATE robot SET agent_reported_hosting_at='2020-01-01 00:00:00' WHERE robot_id=?", robotID,
	).Exec()
	require.NoError(t, err)
	stale := readAgentInfo(t, ctx, robotID).AgentReportedHostingAt
	require.True(t, stale.Valid)

	// 完全相同的 body 再报一次。
	require.Equal(t, http.StatusOK, botRegister(t, ctx, h, botToken, body).Code)

	fresh := readAgentInfo(t, ctx, robotID).AgentReportedHostingAt
	require.True(t, fresh.Valid)
	assert.True(t, fresh.Time.After(stale.Time),
		"hosting 值未变化时时间戳仍须刷新，否则它回答的是「值何时改变」而非「何时收到上报」")
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
	require.Equal(t, http.StatusOK, botRegister(t, ctx, h, botToken, map[string]interface{}{}).Code)
	row := readAgentInfo(t, ctx, robotID)
	assert.Equal(t, "", row.AgentHosting)
	assert.False(t, row.AgentReportedHostingAt.Valid,
		"没上报任何 agent 字段时不得盖时间戳")

	// 完全没有 body（最老的客户端形态）。
	w := httptest.NewRecorder()
	h.ServeHTTP(w, userAPIRequest(t, http.MethodPost, "/v1/bot/register", botToken, nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.False(t, readAgentInfo(t, ctx, robotID).AgentReportedHostingAt.Valid,
		"无 body 的 register 同样不得写 agent_* 列")
}

// TestRegisterVersionOnlyReportDoesNotTouchHosting —— 只报版本时，带外写入的
// agent_hosting 必须存活。
//
// **本测试证明什么、不证明什么**（PR #837 round 4 P1(b) 修正）：它证明"缺席字段的值
// 不会被本次上报改动"这个**可观察结果**。它**不能**区分稀疏写入与
// read-merge-write —— 带外 UPDATE 落在下一次 register **之前**，合并实现读到的已经是
// 新值、再写回去，结果一模一样。真正的交错窗口在 queryRobotByBotToken 与 UPDATE 之间，
// 行为测试无法确定性地插进去。
//
// 那个不变量由 modules/bot_api 的两条**源码守卫**钉住
// （TestApplyAgentReportCannotReadTheRobotRow /
// TestRegisterUserBotPassesStoredOnlyAsASkipSnapshot，已实测能杀掉 read-merge-write 变异），
// 以及 TestUpdateRobotAgentInfoOmitsUnreportedColumns（直接断言发出的 SQL 里有哪些列）。
// 早前这段注释声称本测试"用带外写入逼出区别"，那是错的 —— reviewer 实测在植入合并
// 实现后本测试照样绿。
func TestRegisterVersionOnlyReportDoesNotTouchHosting(t *testing.T) {
	h, ctx := newUserAPITestServer(t)
	resetUIDRateLimit(t, ctx)
	robotID, botToken := setupAgentHostingBot(t, ctx, "hostkeep")

	// 先让 bot 报一次 hosting，落库。
	require.Equal(t, http.StatusOK, botRegister(t, ctx, h, botToken, map[string]interface{}{
		"agent_hosting": "self_hosted",
	}).Code)
	require.Equal(t, bot_api.AgentHostingSelfHosted, readAgentInfo(t, ctx, robotID).AgentHosting)

	// 带外写入：模拟另一个 runtime（或运维）在此期间把 hosting 改掉。写回实现读到的
	// 是上面那个 self_hosted，会把这里的新值覆盖回去。
	const outOfBand = "octo_hosted"
	_, err := ctx.DB().UpdateBySql(
		"UPDATE robot SET agent_hosting=? WHERE robot_id=?", outOfBand, robotID,
	).Exec()
	require.NoError(t, err)

	// 只报版本，不带 agent_hosting。
	require.Equal(t, http.StatusOK, botRegister(t, ctx, h, botToken, map[string]interface{}{
		"plugin_version": "2.0.0",
	}).Code)

	row := readAgentInfo(t, ctx, robotID)
	assert.Equal(t, outOfBand, row.AgentHosting,
		"缺席的字段必须不进 UPDATE 语句；写回「读到的旧值」会覆盖并发写入（丢更新）")
	assert.Equal(t, "2.0.0", row.PluginVersion, "本次上报的字段仍应落库")
}

// TestRegisterHostingOnlyReportDoesNotClobberVersions —— 反向的同一个可观察结果：
// 只报 hosting 时，带外写入的版本值必须存活。
//
// 背景是 PR #837 round 1 两位 reviewer 都判为阻塞的 P1-2：初版对 agent_hosting 用了
// 稀疏写入，却对三个 sibling 版本列做了它注释里明确否决的「回写读到的值」，而
// 「只报 hosting」正是本功能新引入的请求形态 —— 等于新开了一条丢更新的路径。
//
// 与上一条同样的边界：本测试证明可观察结果，**不**区分两种实现（同样的原因，
// 带外写入没有落在读写窗口内）。区分交给 bot_api 的源码守卫 + SQL 形状断言。
func TestRegisterHostingOnlyReportDoesNotClobberVersions(t *testing.T) {
	h, ctx := newUserAPITestServer(t)
	resetUIDRateLimit(t, ctx)
	robotID, botToken := setupAgentHostingBot(t, ctx, "hostonly")

	require.Equal(t, http.StatusOK, botRegister(t, ctx, h, botToken, map[string]interface{}{
		"agent_platform": "OpenClaw",
		"agent_version":  "0.1.0",
	}).Code)

	// 带外写入：另一个 runtime 报了新版本。
	_, err := ctx.DB().UpdateBySql(
		"UPDATE robot SET agent_version=? WHERE robot_id=?", "9.9.9", robotID,
	).Exec()
	require.NoError(t, err)

	// 只报 hosting。
	require.Equal(t, http.StatusOK, botRegister(t, ctx, h, botToken, map[string]interface{}{
		"agent_hosting": "self_hosted",
	}).Code)

	row := readAgentInfo(t, ctx, robotID)
	assert.Equal(t, "9.9.9", row.AgentVersion,
		"只报 hosting 不得把版本列写回读到的旧值（PR #837 P1-2）")
	assert.Equal(t, bot_api.AgentHostingSelfHosted, row.AgentHosting)
}

// 时区同源的守卫**不在本文件**。
//
// 这里曾有一条 TestRegisterHostingTimestampSharesMySQLClockWithBoundAt：把会话时区
// 调到 +08:00，再比较 agent_reported_hosting_at 与 NOW() 写的 bound_at。它确实能杀掉
// 「dbr.Expr("NOW()") 换回 time.Now()」这个变异（实测偏差 7h59m59s），但**代价是整包
// 不可跑**：要让 register 的写入落在被改过时区的会话上，就得把 ctx.DB() 的连接池压到
// 单连接，而那是**进程级共享状态** —— 压池期间其它测试的查询排队/超时，成片失败；
// 用 SetConnMaxLifetime 强制作废连接来收尾同样不稳定（改完后失败集每次都不一样）。
//
// 换来的信息量也有限：它验证的是「两个 TIMESTAMP 在非 UTC 会话下不分叉」，而这一点
// 的**充分**条件就是「写入语句里用的是 SQL NOW() 而不是绑定参数」——
// 那是 modules/bot_api 的 TestUpdateRobotAgentInfoStampsHostingTimeWithSQLNow 直接
// 断言的，同样能杀掉同一个变异（已实测），且不碰任何共享状态。
//
// 所以这条被删掉，而不是修好：一条需要污染进程级状态才能运行的测试，在整包里就是
// 不可靠的，而它守的不变量已有确定性的守法。这个取舍写在这里，免得下一个人以为
// 「时区没人管」而重新加回同一个形状。

// TestRegisterEmptyLegacyVersionPreservesStoredValue —— 三个 legacy 版本字段报
// **空串**必须保留已存值，不能清空。这是 PR #837 round 2 的阻塞项（P1-1）。
//
// 回归的来路：为修丢更新把这三个字段从裸 string 改成 *string 之后，任何非 nil 指针
// 都被无条件写入 —— 包括指向空串的。于是在 merge base 上「报空 = 保留」的行为
// 变成了「报空 = 清空」，而当时的注释还断言 wire 契约未变。触发不需要恶意：
// 任何 struct 不带 omitempty 的客户端都会送 ""，而 register 是重连路径，
// 于是每次重连擦一次，HTTP 200、无日志、事后与「从未上报」不可区分。
//
// 两位 reviewer 独立端到端复现了它。这条测试就是他们那个复现。
func TestRegisterEmptyLegacyVersionPreservesStoredValue(t *testing.T) {
	h, ctx := newUserAPITestServer(t)
	resetUIDRateLimit(t, ctx)
	robotID, botToken := setupAgentHostingBot(t, ctx, "hostlegacy")

	// 播种三个值。
	require.Equal(t, http.StatusOK, botRegister(t, ctx, h, botToken, map[string]interface{}{
		"agent_platform": "OpenClaw",
		"agent_version":  "1.2.3",
		"plugin_version": "9.9.9",
	}).Code)

	// 报一个新版本，另两个送空串 —— 正是「序列化器对未填字段输出 ""」的形态。
	require.Equal(t, http.StatusOK, botRegister(t, ctx, h, botToken, map[string]interface{}{
		"agent_platform": "",
		"agent_version":  "1.2.4",
		"plugin_version": "",
	}).Code)

	row := readAgentInfo(t, ctx, robotID)
	assert.Equal(t, "1.2.4", row.AgentVersion, "非空值应更新")
	assert.Equal(t, "OpenClaw", row.AgentPlatform,
		"legacy 字段报空串必须保留已存值（merge base 的契约），不能清空")
	assert.Equal(t, "9.9.9", row.PluginVersion,
		"legacy 字段报空串必须保留已存值（merge base 的契约），不能清空")
}

// TestRegisterAllEmptyLegacyFieldsWritesNothing —— 三个 legacy 字段全报空串时
// 一行都不该写，包括不该盖 hosting 时间戳。
//
// 「supplied」与「writable」在这里分叉：字段确实被送来了（指针非 nil），但对 legacy
// 列而言空串不可写，所以整条语句无内容可发。
func TestRegisterAllEmptyLegacyFieldsWritesNothing(t *testing.T) {
	h, ctx := newUserAPITestServer(t)
	resetUIDRateLimit(t, ctx)
	robotID, botToken := setupAgentHostingBot(t, ctx, "hostnoop")

	require.Equal(t, http.StatusOK, botRegister(t, ctx, h, botToken, map[string]interface{}{
		"agent_platform": "",
		"agent_version":  "",
		"plugin_version": "",
	}).Code)

	row := readAgentInfo(t, ctx, robotID)
	assert.Equal(t, "", row.AgentPlatform)
	assert.False(t, row.AgentReportedHostingAt.Valid,
		"没有可写内容时不得盖 hosting 时间戳")
}

// TestRegisterOversizedBodyDegradesToNoTelemetry —— 超过 bot_api.MaxRegisterBodyBytes
// 的 body 按「未上报」处理：register 仍 200，已存列不受影响。
//
// 这是 PR #837 round 2 的 P2-5：body 上限是本功能唯一没有测试的新行为
// （reviewer 手工验过，但那不在 suite 里）。
func TestRegisterOversizedBodyDegradesToNoTelemetry(t *testing.T) {
	h, ctx := newUserAPITestServer(t)
	resetUIDRateLimit(t, ctx)
	robotID, botToken := setupAgentHostingBot(t, ctx, "hostbig")

	require.Equal(t, http.StatusOK, botRegister(t, ctx, h, botToken, map[string]interface{}{
		"agent_platform": "OpenClaw",
		"agent_hosting":  "self_hosted",
	}).Code)
	before := readAgentInfo(t, ctx, robotID)
	require.Equal(t, bot_api.AgentHostingSelfHosted, before.AgentHosting)

	// 恰好压到上限之内的 body 必须**被采纳** —— 这一半是边界的下侧。
	// 只测"8 KiB 被拒"的话，把上限从 4 KiB 调到 8 KiB 仍然全绿，常量就没被钉住
	// （round 4 P2-6）。
	// 按**序列化后的确切字节数**构造：正好 4096 必须被采纳，4097 必须被拒。
	//
	// **必须用字面量 4096，不能用 bot_api.MaxRegisterBodyBytes。** 用常量表达期望值时，
	// 改常量测试会跟着变 —— 我第一次就是这么写的，把上限改成 8<<10 或 4095 两个变异
	// 都照样绿，等于常量仍然没被钉住（这正是 round 5 P2-6 指出的问题，换个写法并不解决）。
	// 下面先断言常量等于字面量，再用字面量构造 body：这样改常量会红在第一条断言上。
	//
	// **杀掉的变异**：把 MaxRegisterBodyBytes 改成 4096 之外的任何值。
	const wantCap = 4096
	require.Equal(t, wantCap, bot_api.MaxRegisterBodyBytes,
		"register body 上限被改动了。改它本身可能是对的，但请一并确认："+
			"(1) 4 KiB 对四个短字符串仍然宽裕；(2) 下面两条边界断言随之更新")
	exactBody := func(total int) []byte {
		skeleton, err := json.Marshal(map[string]string{"agent_hosting": "octo_hosted", "padding": ""})
		require.NoError(t, err)
		pad := total - len(skeleton)
		require.Greater(t, pad, 0, "目标长度必须大于骨架长度")
		raw, err := json.Marshal(map[string]string{
			"agent_hosting": "octo_hosted",
			"padding":       strings.Repeat("x", pad),
		})
		require.NoError(t, err)
		require.Len(t, raw, total, "构造出的 body 必须正好 %d 字节", total)
		return raw
	}
	postRaw := func(payload []byte) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/bot/register", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+botToken)
		req.Header.Set("Content-Type", "application/json")
		resetRegisterRateLimit(t, ctx)
		h.ServeHTTP(w, req)
		return w
	}

	require.Equal(t, http.StatusOK, postRaw(exactBody(wantCap)).Code)
	assert.Equal(t, bot_api.AgentHostingOctoHosted, readAgentInfo(t, ctx, robotID).AgentHosting,
		"正好 %d 字节的 body 必须被采纳 —— MaxBytesReader 的上限含端点", wantCap)

	// 复位，再验超一字节。
	require.Equal(t, http.StatusOK, botRegister(t, ctx, h, botToken, map[string]interface{}{
		"agent_hosting": "self_hosted",
	}).Code)
	require.Equal(t, bot_api.AgentHostingSelfHosted, readAgentInfo(t, ctx, robotID).AgentHosting)

	require.Equal(t, http.StatusOK, postRaw(exactBody(wantCap+1)).Code,
		"超限也不得让 register 失败")
	assert.Equal(t, bot_api.AgentHostingSelfHosted, readAgentInfo(t, ctx, robotID).AgentHosting,
		"超上限 1 字节的 body 必须被整体丢弃，已存值不变")

}

// TestRegisterHostingNoneClearsTheShape —— 撤回走保留 slug，不走空串。
//
// 决策理由（round 4 P2-4）：同一个 JSON 里 `""` 对三个 legacy 字段是「保持」，
// 若对第四个是「清空」，就成了客户端作者的陷阱；而且"从不填该字段但总是发这个 key"
// 的客户端会每次重连落进 `(”, 非NULL)` —— 那个状态被三处文档定义为「曾上报后显式
// 清空」，等于把序列化器默认值读成了刻意撤回。保留 slug 让撤回显式且可 grep。
func TestRegisterHostingNoneClearsTheShape(t *testing.T) {
	h, ctx := newUserAPITestServer(t)
	resetUIDRateLimit(t, ctx)
	robotID, botToken := setupAgentHostingBot(t, ctx, "hostnone2")

	require.Equal(t, http.StatusOK, botRegister(t, ctx, h, botToken, map[string]interface{}{
		"agent_hosting": "self_hosted",
	}).Code)
	require.Equal(t, bot_api.AgentHostingSelfHosted, readAgentInfo(t, ctx, robotID).AgentHosting)

	// "" 是「未上报」：值必须保持不变。
	require.Equal(t, http.StatusOK, botRegister(t, ctx, h, botToken, map[string]interface{}{
		"agent_hosting": "",
	}).Code)
	assert.Equal(t, bot_api.AgentHostingSelfHosted, readAgentInfo(t, ctx, robotID).AgentHosting,
		"空串是「未上报」，与三个 legacy 字段同口径，不得清空")

	// none 才是撤回。
	require.Equal(t, http.StatusOK, botRegister(t, ctx, h, botToken, map[string]interface{}{
		"agent_hosting": bot_api.AgentHostingNone,
	}).Code)
	row := readAgentInfo(t, ctx, robotID)
	assert.Equal(t, "", row.AgentHosting, "none 必须把形态撤回成空")
	assert.True(t, row.AgentReportedHostingAt.Valid,
		"撤回是一次真实上报，时间戳必须非 NULL —— ('', 非NULL) 正是「曾上报后被清空」")
}

// TestRegisterPartiallyMalformedBodyAdoptsNothing —— 类型错误的 body 一个字段都
// 不采纳（全有或全无）。
//
// PR #837 round 2 的 P2-4：json.Decoder 会先填好已解析的字段再返回类型错误，
// 所以「忽略 bind 错误」等于采纳一个前缀 —— 下面这个 body 会存下 platform、
// 丢掉后面，且没有任何诊断。半更新的列比不更新更糟：它看起来像一次成功上报。
func TestRegisterPartiallyMalformedBodyAdoptsNothing(t *testing.T) {
	h, ctx := newUserAPITestServer(t)
	resetUIDRateLimit(t, ctx)
	robotID, botToken := setupAgentHostingBot(t, ctx, "hostpart")

	// 不能用 map 造这个 body（会被 json.Marshal 修正），直接发原始字节。
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/bot/register",
		strings.NewReader(`{"agent_platform":"OpenClaw","agent_version":12345}`))
	req.Header.Set("Authorization", "Bearer "+botToken)
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "畸形 body 不得让 register 失败")

	row := readAgentInfo(t, ctx, robotID)
	assert.Equal(t, "", row.AgentPlatform,
		"类型错误的 body 一个字段都不该被采纳 —— 半更新看起来像成功上报")

	// 尾随垃圾同理（一个合法值后面跟别的东西）。
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/bot/register",
		strings.NewReader(`{"agent_platform":"OpenClaw"} trailing`))
	req2.Header.Set("Authorization", "Bearer "+botToken)
	req2.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(w2, req2)
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "", readAgentInfo(t, ctx, robotID).AgentPlatform,
		"尾随垃圾同样不得被部分采纳")
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

	w := botRegister(t, ctx, h, token, map[string]interface{}{
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
// agent_reported_hosting_at 显式 null（与 bound_at 一致）。
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
		"UPDATE robot SET agent_hosting='octo_hosted', agent_reported_hosting_at=NOW() WHERE robot_id=?", reported,
	).Exec()
	require.NoError(t, err)

	key := mintUserAPIKey(t, ctx, uid)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, userAPIRequest(t, http.MethodGet, "/v1/user/bots", key, nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// **按 robot_id 解码后逐个断言**，不用整体子串匹配。
	//
	// 上一版对整个响应做 Contains(`"agent_reported_hosting_at":null`) —— 沉默那个 bot
	// 单独就满足它，于是**已上报**那个 bot 的时间戳从未被检查：删掉
	// api_user.go 里的 AgentReportedHostingAt 映射，套件照样绿，目标 2 的一半是无守卫
	// 上线的（PR #837 round 5 🟡，reviewer 实测）。
	//
	// **杀掉的变异**：删除 modules/botfather/api_user.go 里 agentReportedHostingAt 的
	// 计算与赋值。
	var items []struct {
		RobotID                string  `json:"robot_id"`
		AgentHosting           *string `json:"agent_hosting"`
		AgentReportedHostingAt *string `json:"agent_reported_hosting_at"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &items))

	byID := map[string]int{}
	for i, it := range items {
		byID[it.RobotID] = i
	}
	ri, ok := byID[reported]
	require.True(t, ok, "已上报的 Bot 应出现在列表里")
	si, ok := byID[silent]
	require.True(t, ok, "未上报的 Bot 也应出现在列表里")

	// 已上报的那个：形态与时间戳都必须带出。
	require.NotNil(t, items[ri].AgentHosting, "已上报的 Bot 必须带出 agent_hosting")
	assert.Equal(t, bot_api.AgentHostingOctoHosted, *items[ri].AgentHosting)
	assert.NotNil(t, items[ri].AgentReportedHostingAt,
		"已上报的 Bot 必须带出 agent_reported_hosting_at —— 缺了它 agent_hosting 就是个"+
			"无从判断新鲜度的裸值，而这正是加这一列的理由")

	// 未上报的那个：agent_hosting 被 omitempty 省略（解码成 nil），时间戳显式 null。
	assert.Nil(t, items[si].AgentHosting,
		"未上报时 agent_hosting 应被 omitempty 省略，而不是下发空串")
	assert.Nil(t, items[si].AgentReportedHostingAt,
		"未上报时 agent_reported_hosting_at 必须是 null（与 bound_at 同口径）")

	// 字段确实出现在 JSON 里（而不是连键都没有）—— omitempty 只作用于 hosting。
	assert.Contains(t, w.Body.String(), `"agent_reported_hosting_at"`,
		"agent_reported_hosting_at 不得带 omitempty：null 与省略对调用方含义不同")
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

	// 本测试断言的行为**只在严格模式下成立**：非严格模式会静默截断而不是报 1406，
	// 于是超长值会写进去、整组 UPDATE 也不失败。MySQL 8 默认严格，但显式检查比
	// 依赖默认值可靠 —— 否则在一个宽松配置的库上这条会以「行为变了」的假象红掉。
	var sqlMode string
	require.NoError(t, ctx.DB().SelectBySql("SELECT @@SESSION.sql_mode").LoadOne(&sqlMode))
	if !strings.Contains(sqlMode, "STRICT_TRANS_TABLES") && !strings.Contains(sqlMode, "STRICT_ALL_TABLES") {
		t.Skipf("本测试要求严格模式（当前 sql_mode=%q）：非严格模式下超长值被静默截断，整组 UPDATE 不会失败", sqlMode)
	}

	require.Equal(t, http.StatusOK, botRegister(t, ctx, h, botToken, map[string]interface{}{
		"agent_platform": strings.Repeat("x", 200), // 列宽 50
		"agent_hosting":  "vendor_hosted",          // 本身完全合法
	}).Code, "UPDATE 失败只记日志，register 仍须返回 200")

	row := readAgentInfo(t, ctx, robotID)
	assert.Equal(t, "", row.AgentPlatform, "超长值本就写不进 VARCHAR(50)")
	assert.Equal(t, "", row.AgentHosting,
		"已知限制：同一条 UPDATE 里的合法 agent_hosting 被超长的 agent_platform 连带挡住")
	assert.False(t, row.AgentReportedHostingAt.Valid, "整条 UPDATE 失败，时间戳同样没落下")
}

// TestAgentHostingMigrationIsSingleAtomicAlter —— 两列必须在同一条 ALTER 里。
//
// **单条 ALTER 只解决一件事**：不留「一列已加、一列没加」的列级中间态。
//
// 它**不解决**可重入性 —— 这一点上一版的注释说错了（round 4 P2-3）。
// 20260603000002 那条存在性守卫防的是「两个 pod 竞争同一迁移」和「DDL 隐式提交后、
// gorp_migrations 记账前进程死掉」，单条原子 ALTER 对这两者都无效。
//
// **本迁移不加守卫，这是遵循本仓既定原则，不是本任务的自由裁量**：
// sql-migrate 已用 `gorp_migrations` 追踪每个文件的版本，所以不要在每条迁移里堆幂等
// 代码 —— 存在性守卫是"同一份迁移必须跨多个状态不同的环境运行"时的应急路径。
// 实测口径（只数 Up 段）：全仓 83 个含 ADD COLUMN 的迁移里只有 14 个带守卫，且集中在该原则确立之前
// （2026-06 前后）；此后的 20260728 / 20260810 / 20260830 都是裸 DDL。
//
// reviewer 提出的两-pod 竞争风险是真实的，但它的解法在**部署层**（迁移加锁或单 pod
// 执行），不是让每条迁移各自重复一遍幂等逻辑 —— 后者可读性差、reviewer 看不出真实
// 意图，正是那条原则要避免的。
//
// 本测试因此**不断言**「不得出现 INFORMATION_SCHEMA / CREATE PROCEDURE」：若将来
// 部署形态确实需要守卫，加守卫是正当的，不该被一条测试挡住。
func TestAgentHostingMigrationIsSingleAtomicAlter(t *testing.T) {
	raw, err := os.ReadFile("sql/20260903000001_botfather_agent_hosting.sql")
	require.NoError(t, err)
	src := string(raw)

	up := src[strings.Index(src, "-- +migrate Up"):strings.Index(src, "-- +migrate Down")]
	assert.Equal(t, 1, strings.Count(up, "ALTER TABLE"),
		"Up 段必须只有一条 ALTER TABLE（两列同批加，避免列级半应用态）")
	assert.Contains(t, up, "agent_hosting")
	assert.Contains(t, up, "agent_reported_hosting_at")
	assert.Contains(t, up, "不可用于鉴权",
		"列 COMMENT 必须写明自报值不可用于鉴权")
}
