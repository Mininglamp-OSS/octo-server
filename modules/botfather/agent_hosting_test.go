package botfather

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

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

// TestRegisterStoresAgentHosting —— 正常上报落库，且盖上 hosting 上报时间戳。
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
// 清空仍然可做，但要显式报 ""（那是格式良好的上报，不是被拒的上报）。
//
// 起点是一个**已有合法值**的 bot —— 初版测试从「从未上报」起步，区分不了
// 「存了空串」和「本来就空」，所以完全覆盖不到这个场景。
func TestRegisterMalformedHostingPreservesStoredValue(t *testing.T) {
	h, ctx := newUserAPITestServer(t)
	resetUIDRateLimit(t, ctx)
	robotID, botToken := setupAgentHostingBot(t, ctx, "hostbad")

	require.Equal(t, http.StatusOK, botRegister(t, h, botToken, map[string]interface{}{
		"agent_hosting": "octo_hosted",
	}).Code)
	require.Equal(t, bot_api.AgentHostingOctoHosted, readAgentInfo(t, ctx, robotID).AgentHosting)

	for _, bad := range []string{
		`<script>alert(1)</script>`, // caller-controlled 字节
		"self-hosted",               // 现实中最可能的拼错：连字符
		"K_hosted",                  // U+212A KELVIN SIGN，ToLower 会折成 'k'
	} {
		w := botRegister(t, h, botToken, map[string]interface{}{
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

// TestRegisterExplicitEmptyHostingClears —— 与上一条成对：显式报 "" 是格式良好的
// 上报，含义是「清空」，与「被拒」区分开。
func TestRegisterExplicitEmptyHostingClears(t *testing.T) {
	h, ctx := newUserAPITestServer(t)
	resetUIDRateLimit(t, ctx)
	robotID, botToken := setupAgentHostingBot(t, ctx, "hostclr")

	require.Equal(t, http.StatusOK, botRegister(t, h, botToken, map[string]interface{}{
		"agent_hosting": "octo_hosted",
	}).Code)
	require.Equal(t, bot_api.AgentHostingOctoHosted, readAgentInfo(t, ctx, robotID).AgentHosting)

	require.Equal(t, http.StatusOK, botRegister(t, h, botToken, map[string]interface{}{
		"agent_hosting": "",
	}).Code)
	assert.Equal(t, "", readAgentInfo(t, ctx, robotID).AgentHosting,
		"显式空串是「清空」，与被拒的非法值必须区分")
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
// 四个字段现在都是指针（PR #837 review 后统一），所以「缺席 vs 报了空」对每个字段
// 都可区分。hosting 与三个版本字段的差别只在于：对版本号「报空」没有意义，而这里
// 「报空」正是 runtime 清掉陈旧形态的方式 —— 自运维→平台托管切换时陈旧值比空值更有害。
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
		"UPDATE robot SET agent_reported_hosting_at='2020-01-01 00:00:00' WHERE robot_id=?", robotID,
	).Exec()
	require.NoError(t, err)
	stale := readAgentInfo(t, ctx, robotID).AgentReportedHostingAt
	require.True(t, stale.Valid)

	// 完全相同的 body 再报一次。
	require.Equal(t, http.StatusOK, botRegister(t, h, botToken, body).Code)

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
	require.Equal(t, http.StatusOK, botRegister(t, h, botToken, map[string]interface{}{}).Code)
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

// TestRegisterVersionOnlyReportDoesNotTouchHosting —— 只报版本时，agent_hosting
// 列必须**不在 UPDATE 语句里**，而不是「被写回同一个值」。
//
// 这两者的区别不是风格问题，是并发正确性：写回实现会丢更新（A 读旧 → B 写新 →
// A 把旧值写回）。用**带外写入**逼出这个区别 —— 在 register 读到行之后、写回之前
// 由第三方改掉 hosting，写回实现会把它覆盖成读到的旧值，稀疏实现则原样保留。
//
// 这条替换了初版的写法：初版只断言「值保留 + 时间戳前进」，而在被否决的写回实现下
// 语句里既有 `agent_hosting=<旧值>` 也有时间戳，三条断言全过 —— 论证不成立
// （PR #837 两位 reviewer 独立指出同一点）。真正的区分靠下面的 out-of-band 写入，
// 以及同包的 TestUpdateRobotAgentInfoOmitsUnreportedColumns（直接断言 SQL）。
func TestRegisterVersionOnlyReportDoesNotTouchHosting(t *testing.T) {
	h, ctx := newUserAPITestServer(t)
	resetUIDRateLimit(t, ctx)
	robotID, botToken := setupAgentHostingBot(t, ctx, "hostkeep")

	// 先让 bot 报一次 hosting，落库。
	require.Equal(t, http.StatusOK, botRegister(t, h, botToken, map[string]interface{}{
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
	require.Equal(t, http.StatusOK, botRegister(t, h, botToken, map[string]interface{}{
		"plugin_version": "2.0.0",
	}).Code)

	row := readAgentInfo(t, ctx, robotID)
	assert.Equal(t, outOfBand, row.AgentHosting,
		"缺席的字段必须不进 UPDATE 语句；写回「读到的旧值」会覆盖并发写入（丢更新）")
	assert.Equal(t, "2.0.0", row.PluginVersion, "本次上报的字段仍应落库")
}

// TestRegisterHostingOnlyReportDoesNotClobberVersions —— 反向的同一个不变量：
// 只报 hosting 时三个版本列必须不在语句里。
//
// 这是 PR #837 两位 reviewer 都判为阻塞的那条（P1-2）：初版对 agent_hosting 用了
// 稀疏写入，却对三个 sibling 版本列做了它注释里明确否决的「回写读到的值」，而且
// 「只报 hosting」正是本功能新引入的请求形态 —— 等于新开了一条丢更新的路径。
func TestRegisterHostingOnlyReportDoesNotClobberVersions(t *testing.T) {
	h, ctx := newUserAPITestServer(t)
	resetUIDRateLimit(t, ctx)
	robotID, botToken := setupAgentHostingBot(t, ctx, "hostonly")

	require.Equal(t, http.StatusOK, botRegister(t, h, botToken, map[string]interface{}{
		"agent_platform": "OpenClaw",
		"agent_version":  "0.1.0",
	}).Code)

	// 带外写入：另一个 runtime 报了新版本。
	_, err := ctx.DB().UpdateBySql(
		"UPDATE robot SET agent_version=? WHERE robot_id=?", "9.9.9", robotID,
	).Exec()
	require.NoError(t, err)

	// 只报 hosting。
	require.Equal(t, http.StatusOK, botRegister(t, h, botToken, map[string]interface{}{
		"agent_hosting": "self_hosted",
	}).Code)

	row := readAgentInfo(t, ctx, robotID)
	assert.Equal(t, "9.9.9", row.AgentVersion,
		"只报 hosting 不得把版本列写回读到的旧值（PR #837 P1-2）")
	assert.Equal(t, bot_api.AgentHostingSelfHosted, row.AgentHosting)
}

// TestRegisterHostingTimestampSharesMySQLClockWithBoundAt —— 时间戳必须与
// bound_at 同源（都由 SQL NOW() 写），把两种时钟放进**同一个断言**。
//
// 这是 PR #837 两位 reviewer 都判为阻塞的另一条（P1-1）：初版用 Go 侧 time.Now()，
// 经驱动 Config.Loc（默认 UTC，DSN 未设 loc）转换，而应用镜像固定
// TZ=Asia/Shanghai —— MySQL session 时区非 UTC 时，同一响应里这两个时间戳会相差
// 8 小时且无标记解释。生产 MySQL 是 UTC 所以那是潜伏而非已发生，但初版的测试
// **结构上**抓不到它：要么两个 Go 写的值互比（同一时钟内单调），要么用 SQL NOW()
// 播种（从不把两种来源放进同一个比较）。
//
// 本测试在本地 UTC MySQL 下同样能红：它不依赖时区偏移，而是断言两个时间戳彼此
// 接近；Go 侧写入一旦回来，只要 DB 时区非 UTC 就会差出小时级。为了在**任何**
// 时区下都能抓到，显式把 session 时区设成 +08:00 再比。
func TestRegisterHostingTimestampSharesMySQLClockWithBoundAt(t *testing.T) {
	h, ctx := newUserAPITestServer(t)
	resetUIDRateLimit(t, ctx)
	robotID, botToken := setupAgentHostingBot(t, ctx, "hosttz")

	// 用**独占一条连接**做时区偏移，不要打在池上（ctx.DB() 是连接池：
	// SET 落在某条连接、deferred 复位可能落在另一条，会把一条 +08:00 的连接留在
	// 池里污染后续测试。PR #837 round 2 P2-6）。
	conn, err := ctx.DB().DB.Conn(context.Background())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }() // 关闭即释放，池里不留被改过的连接

	if _, err := conn.ExecContext(context.Background(), "SET time_zone = '+08:00'"); err != nil {
		t.Skipf("无法设置 session 时区（权限/后端差异），跳过：%v", err)
	}

	require.Equal(t, http.StatusOK, botRegister(t, h, botToken, map[string]interface{}{
		"agent_hosting": "self_hosted",
	}).Code)

	// 参照物：在**这条**偏移过的连接上用 NOW() 写 bound_at —— 生产就是这么写的
	// （modules/botfather/db.go 的 bindRobotCAS）。register 那次写入走的是池里
	// 另一条连接，所以如果实现用 Go 侧 time.Now()，它会被驱动按 Config.Loc(UTC)
	// 转换，与这里的 NOW() 差出整个时区偏移；用 SQL NOW() 则两者同源。
	_, err = conn.ExecContext(context.Background(),
		"UPDATE robot SET bound_at=NOW() WHERE robot_id=?", robotID)
	require.NoError(t, err)

	var row struct {
		HostingAt dbr.NullTime `db:"agent_reported_hosting_at"`
		BoundAt   dbr.NullTime `db:"bound_at"`
	}
	require.NoError(t, ctx.DB().SelectBySql(
		"SELECT agent_reported_hosting_at, bound_at FROM robot WHERE robot_id=?", robotID,
	).LoadOne(&row))
	require.True(t, row.HostingAt.Valid)
	require.True(t, row.BoundAt.Valid)

	skew := row.BoundAt.Time.Sub(row.HostingAt.Time)
	if skew < 0 {
		skew = -skew
	}
	assert.Less(t, skew, time.Minute,
		"agent_reported_hosting_at 与 bound_at 必须同源（都用 SQL NOW()）；"+
			"实测偏差 %s —— Go 侧 time.Now() 经驱动 Config.Loc(UTC) 转换，"+
			"在非 UTC 的 MySQL session 上会与 NOW() 差出时区偏移（PR #837 P1-1）", skew)
}

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
	require.Equal(t, http.StatusOK, botRegister(t, h, botToken, map[string]interface{}{
		"agent_platform": "OpenClaw",
		"agent_version":  "1.2.3",
		"plugin_version": "9.9.9",
	}).Code)

	// 报一个新版本，另两个送空串 —— 正是「序列化器对未填字段输出 ""」的形态。
	require.Equal(t, http.StatusOK, botRegister(t, h, botToken, map[string]interface{}{
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

	require.Equal(t, http.StatusOK, botRegister(t, h, botToken, map[string]interface{}{
		"agent_platform": "",
		"agent_version":  "",
		"plugin_version": "",
	}).Code)

	row := readAgentInfo(t, ctx, robotID)
	assert.Equal(t, "", row.AgentPlatform)
	assert.False(t, row.AgentReportedHostingAt.Valid,
		"没有可写内容时不得盖 hosting 时间戳")
}

// TestRegisterOversizedBodyDegradesToNoTelemetry —— 超过 maxRegisterBodyBytes
// 的 body 按「未上报」处理：register 仍 200，已存列不受影响。
//
// 这是 PR #837 round 2 的 P2-5：body 上限是本功能唯一没有测试的新行为
// （reviewer 手工验过，但那不在 suite 里）。
func TestRegisterOversizedBodyDegradesToNoTelemetry(t *testing.T) {
	h, ctx := newUserAPITestServer(t)
	resetUIDRateLimit(t, ctx)
	robotID, botToken := setupAgentHostingBot(t, ctx, "hostbig")

	require.Equal(t, http.StatusOK, botRegister(t, h, botToken, map[string]interface{}{
		"agent_platform": "OpenClaw",
		"agent_hosting":  "self_hosted",
	}).Code)
	before := readAgentInfo(t, ctx, robotID)
	require.Equal(t, bot_api.AgentHostingSelfHosted, before.AgentHosting)

	// 合法 JSON，但远超 4 KiB：hosting 本身是合法值，只是整个 body 被截断。
	require.Equal(t, http.StatusOK, botRegister(t, h, botToken, map[string]interface{}{
		"agent_hosting": "octo_hosted",
		"padding":       strings.Repeat("x", 8<<10),
	}).Code, "body 超限不得让 register 失败 —— 它是 Bot 掉线自愈的唯一通道")

	after := readAgentInfo(t, ctx, robotID)
	assert.Equal(t, before.AgentHosting, after.AgentHosting,
		"超限 body 按未上报处理，已存值不得被改动")
	assert.Equal(t, before.AgentPlatform, after.AgentPlatform)
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

	body := w.Body.String()
	assert.Contains(t, body, `"agent_hosting":"octo_hosted"`, "已上报的 Bot 必须带出托管形态")
	// 未上报的那个：字段省略而不是空串（omitempty，与 agent_platform 同组行为）。
	assert.NotContains(t, body, `"agent_hosting":""`,
		"未上报时 agent_hosting 应被 omitempty 省略，而不是下发空串")
	assert.Contains(t, body, `"agent_reported_hosting_at":null`,
		"未上报时必须显式 null（与 bound_at 同口径），而不是省略字段")
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

	require.Equal(t, http.StatusOK, botRegister(t, h, botToken, map[string]interface{}{
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
	assert.Contains(t, up, "agent_reported_hosting_at")
	assert.NotContains(t, up, "INFORMATION_SCHEMA",
		"朴素 DDL 原则：不要在迁移里堆存在性守卫（单条 ALTER 本身已原子）")
	assert.NotContains(t, up, "CREATE PROCEDURE",
		"朴素 DDL 原则：不要用存储过程包幂等逻辑")
	assert.Contains(t, up, "不可用于鉴权",
		"列 COMMENT 必须写明自报值不可用于鉴权")
}
