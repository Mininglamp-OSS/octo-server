package bot_api

// issue #696 per-bot 限流的集成测试。
//
// 这些用例覆盖的是本次改动的**核心主张**，单测和源码守卫都替代不了：
//   - 隔离性：一个 bot 打满配额不影响同 IP 的其它 bot（事故的直接成因）
//   - 自愈链路：业务配额打满时 heartbeat 与 register 仍然可用（否则掉线无法恢复）
//   - 影子模式：dry-run 不改变任何可观察行为（否则观测到的不是真实流量）
//   - kill switch 与热调：无需重启即可开关/调参（否则复制事故的次生伤害模式）
//
// 依赖 MySQL + Redis（authBot 查 robot 表，令牌桶在 Redis）。不依赖 WuKongIM：
// 用例选的端点要么在限流层就返回，要么是只读查询。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/Mininglamp-OSS/octo-server/pkg/ratelimit"
	rd "github.com/go-redis/redis"
	"github.com/stretchr/testify/require"
)

const (
	rlBotA      = "bot_rl_alpha"
	rlBotATok   = "bf_rl_alpha_token"
	rlBotB      = "bot_rl_beta"
	rlBotBTok   = "bf_rl_beta_token"
	rlUnknownTk = "bf_rl_no_such_token"
)

// setupRateLimitTest 建 server、插两个 bot、清限流桶。
//
// 清桶是必须的：令牌桶存在 Redis，`CleanAllTables` 只清 MySQL。不清的话本用例会
// 继承上一个用例（甚至上一次 go test 运行）残留的 token 数，断言随执行顺序漂移。
// 这正是 modules/category 的 resetUIDRateLimit 存在的原因，同一个坑。
func setupRateLimitTest(t *testing.T) (http.Handler, *config.Context) {
	t.Helper()
	s, ctx := testutil.NewTestServer()
	// testutil 不装 i18n ErrorRenderer，不补这一行的话 c.RenderError 会走 octo-lib
	// 的旧兜底 envelope，响应里只有 {msg,status}、断不到 error.code。
	s.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	require.NoError(t, testutil.CleanAllTables(ctx))

	for _, b := range []struct{ id, token string }{{rlBotA, rlBotATok}, {rlBotB, rlBotBTok}} {
		_, err := ctx.DB().InsertBySql(
			"INSERT INTO robot (robot_id, status, creator_uid, bot_token) VALUES (?, 1, ?, ?)",
			b.id, "owner_rl", b.token,
		).Exec()
		require.NoError(t, err)
	}

	resetBotRateLimitBuckets(t, ctx)
	t.Cleanup(func() {
		resetBotRateLimitBuckets(t, ctx)
		SetRateLimitParamsProvider(disabledRateLimitParams)
	})
	return s.GetRoute(), ctx
}

func resetBotRateLimitBuckets(t *testing.T, ctx *config.Context) {
	t.Helper()
	c := rd.NewClient(&rd.Options{
		Addr:     ctx.GetConfig().DB.RedisAddr,
		Password: ctx.GetConfig().DB.RedisPass,
	})
	defer c.Close()
	// 两个 keyspace 都要清：
	//   ratelimit:bot:*                —— 三条 per-bot 通道 + offenders ZSet
	//   ratelimit:strict:bot_register:*  —— register 的 per-IP strict 桶（lib 中间件）
	//   ratelimit:strict:bot_heartbeat:* —— heartbeat 的 per-IP strict 桶（lib 中间件）
	//
	// 漏掉 strict 那条会造成**跨用例污染**：所有用例的请求都来自同一个测试
	// "client IP"，burst 50 会被几个用例累积耗尽，于是后跑的用例随机拿到 429，
	// 失败点还落在与限流无关的断言上。这类 flaky 极难排查，参见
	// modules/category 的 resetUIDRateLimit 与本仓 test_uid_ratelimit 的教训。
	for _, pattern := range []string{
		"ratelimit:bot:*",
		"ratelimit:strict:bot_register:*",
		"ratelimit:strict:bot_heartbeat:*",
	} {
		keys, err := c.Keys(pattern).Result()
		if err == nil && len(keys) > 0 {
			require.NoError(t, c.Del(keys...).Err())
		}
	}
}

// useParams 安装一份限流配置。返回后立即生效——这本身就是「热调无需重启」的体现。
//
// provider 现在按 class 取值（见 RateLimitParamsProvider），这里用 forClass 适配，
// 让用例仍然能一次性写出三条通道。
func useParams(p RateLimitParams) {
	SetRateLimitParamsProvider(p.forClass)
}

func botGet(t *testing.T, h http.Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", path, nil)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	h.ServeHTTP(w, req)
	return w
}

func botPost(t *testing.T, h http.Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", path, nil)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	h.ServeHTTP(w, req)
	return w
}

// openParams 是"放行"用的宽松配置，用于把某一条通道排除出干扰。
func openParams() ratelimit.Params {
	return ratelimit.Params{Enabled: true, RPS: 1000, Burst: 1000}
}

// TestRateLimitDisabledIsFullyTransparent —— kill switch 的完全旁路语义。
//
// 断言的不只是"没被拒"，还有**没有下发任何 X-RateLimit-* 头**：
// 关闭状态下的响应必须与本次改动前逐字节一致，否则 kill switch 自己就成了
// 一个新的行为变更面，翻开关止损时反而引入未知。
func TestRateLimitDisabledIsFullyTransparent(t *testing.T) {
	h, _ := setupRateLimitTest(t)
	useParams(RateLimitParams{}) // 三条通道全关

	for i := 0; i < 30; i++ {
		w := botGet(t, h, "/v1/bot/groups", rlBotATok)
		require.NotEqual(t, http.StatusTooManyRequests, w.Code, "关闭状态不得限流（第 %d 次）", i)
		require.Empty(t, w.Header().Get("X-RateLimit-Limit"), "关闭状态不得下发限流头")
		require.Empty(t, w.Header().Get("X-RateLimit-Scope"), "关闭状态不得下发限流头")
	}
}

// TestRateLimitDryRunDoesNotAffectClients —— 影子模式。
//
// 配额被设成 1 rps / burst 1，即第 2 个请求起桶必然判定为拒；但因为 dry_run，
// 客户端不应察觉任何差异：不被拒、也**不收到限流头**。
//
// "不收到头"是硬要求而非洁癖：影子期若下发 X-RateLimit-Remaining=0，
// 守规矩的客户端会自行降频，于是观测到的不再是真实流量，
// 而观测的全部意义就是回答"设成这个配额会拒谁"。
func TestRateLimitDryRunDoesNotAffectClients(t *testing.T) {
	h, _ := setupRateLimitTest(t)
	useParams(RateLimitParams{
		Business: ratelimit.Params{Enabled: true, DryRun: true, RPS: 1, Burst: 1},
	})

	for i := 0; i < 10; i++ {
		w := botGet(t, h, "/v1/bot/groups", rlBotATok)
		require.NotEqual(t, http.StatusTooManyRequests, w.Code, "dry-run 不得拦截（第 %d 次）", i)
		require.Empty(t, w.Header().Get("X-RateLimit-Limit"), "dry-run 不得下发限流头（第 %d 次）", i)
		require.Empty(t, w.Header().Get("Retry-After"), "dry-run 不得下发 Retry-After（第 %d 次）", i)
	}
}

// TestRateLimitEnforcedShape —— 执行模式下 429 的线上形状。
//
// 复用 shared 限流码，使 bot 侧 429 与全局/UID 层**完全一致**（同一个 error.code、
// 同一个 retry_after detail），客户端无需为 bot 通道加分支。
func TestRateLimitEnforcedShape(t *testing.T) {
	h, _ := setupRateLimitTest(t)
	useParams(RateLimitParams{
		Business: ratelimit.Params{Enabled: true, RPS: 1, Burst: 2},
	})

	var limited *httptest.ResponseRecorder
	for i := 0; i < 10; i++ {
		w := botGet(t, h, "/v1/bot/groups", rlBotATok)
		if w.Code == http.StatusTooManyRequests {
			limited = w
			break
		}
	}
	require.NotNil(t, limited, "burst=2 时连发 10 次必须触发限流")

	// 四个头齐全：X-RateLimit-Scope 是客户端归因 429 来源的唯一手段。
	require.Equal(t, "2", limited.Header().Get("X-RateLimit-Limit"))
	require.Equal(t, "bot", limited.Header().Get("X-RateLimit-Scope"))
	require.NotEmpty(t, limited.Header().Get("X-RateLimit-Remaining"))
	require.NotEmpty(t, limited.Header().Get("Retry-After"))

	// 状态码必须是真正的 429：客户端按状态码识别限流并决定退避。
	// 默认的 ResponseErrorL 门面会把它钉成 400，那样限流会被误判成参数错误。
	require.Equal(t, http.StatusTooManyRequests, limited.Code)

	var body struct {
		Error struct {
			Code       string         `json:"code"`
			HTTPStatus int            `json:"http_status"`
			Details    map[string]any `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(limited.Body.Bytes(), &body))
	require.Equal(t, "err.shared.rate.limited", body.Error.Code,
		"429 必须复用 shared 限流码，与全局/UID 层一致: %s", limited.Body.String())
	require.Equal(t, http.StatusTooManyRequests, body.Error.HTTPStatus)
	require.Contains(t, body.Error.Details, "retry_after",
		"retry_after 是 SafeDetailKeys 白名单内的字段，必须透出给客户端")
}

// TestRateLimitIsolatesBots —— **本次改动的核心主张**。
//
// 事故的成因是限流按出网 IP 分片，于是同一台机器上的所有 bot 共享一份配额，
// 一个 bot 打满就把邻居全部连坐。这条用例直接断言那个成因已被消除：
// bot A 打满自己的桶之后，bot B 必须完全不受影响。
//
// 只断言"A 被限流了"是不够的——那在按 IP 限流时同样成立。必须断言 B 没事。
func TestRateLimitIsolatesBots(t *testing.T) {
	h, _ := setupRateLimitTest(t)
	useParams(RateLimitParams{
		Business: ratelimit.Params{Enabled: true, RPS: 1, Burst: 2},
	})

	var aLimited bool
	for i := 0; i < 10; i++ {
		if botGet(t, h, "/v1/bot/groups", rlBotATok).Code == http.StatusTooManyRequests {
			aLimited = true
		}
	}
	require.True(t, aLimited, "bot A 应当已打满自己的配额")

	// 同一个进程、同一个"客户端 IP"、同一条路由——唯一的差别是 bot 身份。
	w := botGet(t, h, "/v1/bot/groups", rlBotBTok)
	require.NotEqual(t, http.StatusTooManyRequests, w.Code,
		"bot B 被 bot A 的流量连坐了——限流维度仍然不是 bot 身份（issue #696 的原始故障）")
}

// TestSelfHealChannelsSurviveBusinessExhaustion —— **死锁链不可复现**。
//
// 事故的完整链条是：配额打满 → heartbeat 429 → 心跳 key(TTL 60s)过期 → 连接失效
// → 客户端重连 → 重连要调 register 刷 token → register 也被同一份配额拒 → 永远回不来。
//
// 保住心跳但不保住 register，等于保住了"发现自己掉线"的能力却没保住"爬起来"的能力，
// 最终结果一样是断联。所以这条用例必须**同时**断言两者，缺一不可。
func TestSelfHealChannelsSurviveBusinessExhaustion(t *testing.T) {
	h, _ := setupRateLimitTest(t)
	useParams(RateLimitParams{
		// 业务配额极紧，且处于执行模式。
		Business: ratelimit.Params{Enabled: true, RPS: 1, Burst: 1},
		// 两条自愈通道有各自独立的、宽松的配额。
		Heartbeat: openParams(),
		Register:  openParams(),
	})

	var businessLimited bool
	for i := 0; i < 10; i++ {
		if botGet(t, h, "/v1/bot/groups", rlBotATok).Code == http.StatusTooManyRequests {
			businessLimited = true
		}
	}
	require.True(t, businessLimited, "业务配额应当已被打满")

	hb := botPost(t, h, "/v1/bot/heartbeat", rlBotATok)
	require.NotEqual(t, http.StatusTooManyRequests, hb.Code,
		"业务流量打满后心跳被连带拒绝——保活通道没有独立配额（issue #696）")

	reg := botPost(t, h, "/v1/bot/register", rlBotATok)
	require.NotEqual(t, http.StatusTooManyRequests, reg.Code,
		"业务流量打满后 register 被连带拒绝——掉线后无法自愈（issue #696 二次事故）")
}

// TestRegisterLimitedByTokenFingerprint —— register 的限流维度。
//
// register 跑在 authBot 之前、没有 bot 身份，因此按 token 指纹分桶。这正好命中
// 事故里的形态：token 失效的 bot 以约 4 rps 持续重试 register（3 分钟 709 次 400）。
//
// 用一个**无效** token 打：限流未触发时 handler 返回 4xx（token 查不到 robot），
// 触发后应变成 429——即限流层确实在 handler 之前生效，无效 token 的重试风暴
// 会被它自己的桶挡住，而不是每次都穿透到 DB 查询。
func TestRegisterLimitedByTokenFingerprint(t *testing.T) {
	h, _ := setupRateLimitTest(t)
	useParams(RateLimitParams{
		Register: ratelimit.Params{Enabled: true, RPS: 1, Burst: 2},
	})

	var got429 bool
	for i := 0; i < 10; i++ {
		if botPost(t, h, "/v1/bot/register", rlUnknownTk).Code == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	require.True(t, got429, "无效 token 的重试应当被 register 桶挡住")

	// 另一个 token 必须落到不同的桶：一个 bot 的失效重试不该让别人注册不上。
	// 这里用 bot B 的有效 token，断言它没有被上面的风暴连坐。
	w := botPost(t, h, "/v1/bot/register", rlBotBTok)
	require.NotEqual(t, http.StatusTooManyRequests, w.Code,
		"另一个 token 被连坐了——register 的限流维度不是 token")
}

// TestRegisterIPLimitBoundsKeyspaceGrowth —— code review 补入的一层。
//
// 单靠 token 指纹分桶有 keyspace 放大漏洞：token 由客户端提供、限流前不校验有效性，
// 每换一个随机 token 就换一个新桶，而令牌桶 Lua 首次判定即 HMSET+EXPIRE 建 key，
// 于是 live key 数 = 请求速率 × TTL。生产 Redis **未设 maxmemory**（无 LRU 淘汰、
// OOM 直接被 OS kill），所以这不只是"绕过限流"，是可被用来撑内存。
//
// register 前面那道 per-IP strict 桶就是堵这个的：即使每次换 token，同一 IP 的请求
// 速率也被压住，key 生成速率随之受限（100 rps × TTL 约 40s ≈ 4000 个，相对全局桶
// 1500 rps 下最坏约 6 万个收敛一个量级）。
//
// 用例刻意**每次换一个新 token**——这是要证明的核心：若 IP 层缺失，轮换 token 的请求
// 会全部穿透（每个都是全新的桶、都有令牌），永远不会出现 429。
//
// **不靠"连打 N 次撞上限"来触发。** 初版写死 200 次,默认 burst 从 50 提到 500 后它就
// 变红了——而被测行为一点没变,红的只是测试对配额数值的耦合。改为预抽干 IP 桶:
// 断言的是"这道桶在 register 的链路上、且轮换 token 绕不过它",与配额取值无关。
func TestRegisterIPLimitBoundsKeyspaceGrowth(t *testing.T) {
	h, ctx := setupRateLimitTest(t)
	// per-token 通道整个关掉，确保观察到的 429 只能来自 IP 层。
	useParams(RateLimitParams{})

	// 对照:桶未耗尽时,新 token 的 register 应当走到 handler 并被业务逻辑拒绝
	// （token 查不到 ⇒ 400），而不是 429。这条让上面的断言可归因——
	// 否则一个"永远 429"的实现也能让下面通过。
	first := botPost(t, h, "/v1/bot/register", "bf_rotating_probe")
	require.NotEqual(t, http.StatusTooManyRequests, first.Code,
		"桶未耗尽时不应限流——否则是误伤,且下面的断言失去归因能力")

	drainIPBucket(t, ctx, registerIPBucketKey(unknownIP))

	limited := botPost(t, h, "/v1/bot/register", "bf_rotating_after_drain")
	require.Equal(t, http.StatusTooManyRequests, limited.Code,
		"轮换 token 的 register 洪水未被拦住——per-IP strict 桶缺失,"+
			"攻击者可用随机 token 无界增长 Redis keyspace")
	require.Equal(t, "strict:bot_register", limited.Header().Get("X-RateLimit-Scope"),
		"429 应来自 per-token 桶之前的 strict IP 层（lib 中间件写的 scope）")
}

// TestRateLimitHotReload —— 热调无需重启。
//
// 这是本次改动相对 octo-lib 中间件的核心差异，也是对事故次生伤害的直接回应：
// 当时把全局配额从 500 调到 1500 只能靠滚动重启，93 秒窗口内新旧副本共用同一个
// Redis 桶却各传各的参数，行为抖动，一个 bot 恰在此窗口被踢下线。
func TestRateLimitHotReload(t *testing.T) {
	h, ctx := setupRateLimitTest(t)

	// 先用极紧配额打满。
	useParams(RateLimitParams{Business: ratelimit.Params{Enabled: true, RPS: 1, Burst: 1}})
	var limited bool
	for i := 0; i < 10; i++ {
		if botGet(t, h, "/v1/bot/groups", rlBotATok).Code == http.StatusTooManyRequests {
			limited = true
		}
	}
	require.True(t, limited, "紧配额下应当被限流")

	// 不重启、不重建任何东西——只换配置。桶里的 token 还是空的，
	// 所以顺带清一次，验证的是"新配额被读到了"而非"旧桶恰好回填了"。
	resetBotRateLimitBuckets(t, ctx)
	useParams(RateLimitParams{Business: openParams()})

	for i := 0; i < 20; i++ {
		w := botGet(t, h, "/v1/bot/groups", rlBotATok)
		require.NotEqual(t, http.StatusTooManyRequests, w.Code,
			"放宽配额后仍被限流——配置不是每次判定时读的（第 %d 次）", i)
	}

	// 反向：再收紧，应当立即重新开始限流。
	useParams(RateLimitParams{Business: ratelimit.Params{Enabled: true, RPS: 1, Burst: 1}})
	var reLimited bool
	for i := 0; i < 10; i++ {
		if botGet(t, h, "/v1/bot/groups", rlBotATok).Code == http.StatusTooManyRequests {
			reLimited = true
		}
	}
	require.True(t, reLimited, "收紧配额后未立即生效——热调是单向的？")
}

// TestRateLimitSanitizesIllegalConfig —— 读侧防御的端到端验证。
//
// 非法配额（rps<=0）不得让限流"静默关闭"，也不得让整条路由 100% 拒绝：
// 前者把一个配置错误伪装成"限流正常但没人超限"，后者直接打死业务。
// 正确行为是回退到代码默认值并**继续限流**。
//
// rps<=0 尤其危险：令牌桶 Lua 里 `rate <= 0` 会直接 return {0,0,1}，即全部拒绝。
func TestRateLimitSanitizesIllegalConfig(t *testing.T) {
	h, _ := setupRateLimitTest(t)
	useParams(RateLimitParams{
		Business: ratelimit.Params{Enabled: true, RPS: 0, Burst: 0}, // 非法
	})

	// fallback 是 rps=20/burst=200（见 newBotRateLimiters），因此少量请求必须全部放行——
	// 若非法值被原样喂给 Lua，这里会全部 429。
	for i := 0; i < 5; i++ {
		w := botGet(t, h, "/v1/bot/groups", rlBotATok)
		require.NotEqual(t, http.StatusTooManyRequests, w.Code,
			"非法配额被原样使用了，整条路由变成 100%% 拒绝（第 %d 次）", i)
	}
}

// TestHeartbeatBucketStillEnforcesLimit —— per-bot 桶对**已鉴权** bot 生效。
//
// ⚠️ 这条用例只覆盖鉴权之后那一半。它原本的注释声称验证了「exclude ≠ 无限制」，
// 那是错的：它用**有效 token**，走的是 authBot 通过之后的 per-bot 桶，
// 因此在「authBot 之前完全没有限流」这个 P1 漏洞存在时**照样通过**——
// 一个假绿的测试，比没有测试更危险，因为它让人以为那条性质已被验证。
//
// 未鉴权那一半由 TestHeartbeatUnauthenticatedFloodIsRateLimited 覆盖
// （strict IP 桶，排在 authBot 之前）。两条合起来才是完整的「exclude 的对价」。
func TestHeartbeatBucketStillEnforcesLimit(t *testing.T) {
	h, _ := setupRateLimitTest(t)
	useParams(RateLimitParams{
		Heartbeat: ratelimit.Params{Enabled: true, RPS: 1, Burst: 2},
	})

	var limited *httptest.ResponseRecorder
	for i := 0; i < 10; i++ {
		w := botPost(t, h, "/v1/bot/heartbeat", rlBotATok)
		if w.Code == http.StatusTooManyRequests {
			limited = w
			break
		}
	}
	require.NotNil(t, limited,
		"heartbeat 自有桶未生效——它已移出全局 per-IP 桶，此时等于完全无上限")
	require.Equal(t, "bot", limited.Header().Get("X-RateLimit-Scope"))
}

// TestDryRunStillExecutesDecision —— 影子模式必须**真的跑判定**。
//
// brief Acceptance 明确要求 dry-run 不能因为"反正不拦"就跳过 Redis：
//  1. 成本要真实暴露，否则关掉 dry_run 那一刻才发现多了一跳 Redis 往返；
//  2. 更重要的是**观测数据必须产生**——影子模式的全部意义就是回答
//     「设成这个配额会拒谁」，判定不跑就什么都答不出来。
//
// 断言方式:检查 offenders ZSet。它只在(影子)拒绝时写,所以名单里出现该 bot
// 就证明判定执行到了"判为拒绝"这一步——而客户端侧同时没有被拦(上一条用例已钉)。
func TestDryRunStillExecutesDecision(t *testing.T) {
	h, ctx := setupRateLimitTest(t)
	useParams(RateLimitParams{
		Business: ratelimit.Params{Enabled: true, DryRun: true, RPS: 1, Burst: 1},
	})

	for i := 0; i < 5; i++ {
		w := botGet(t, h, "/v1/bot/groups", rlBotATok)
		require.NotEqual(t, http.StatusTooManyRequests, w.Code, "dry-run 不得拦截")
	}

	c := rd.NewClient(&rd.Options{
		Addr:     ctx.GetConfig().DB.RedisAddr,
		Password: ctx.GetConfig().DB.RedisPass,
	})
	defer c.Close()

	members, err := c.ZRevRangeWithScores(rateLimitKeyPrefixOffenders+rateLimitClassBusiness, 0, 9).Result()
	require.NoError(t, err)
	require.NotEmpty(t, members,
		"影子模式下 offenders 名单为空——判定被跳过了，"+
			"于是既拿不到观测数据、也没暴露 Redis 成本")

	var found bool
	for _, m := range members {
		if m.Member == rlBotA {
			found = true
			require.Greater(t, m.Score, float64(0))
		}
	}
	require.True(t, found, "offenders 名单里应出现被影子拒绝的 bot %s，实际: %v", rlBotA, members)
}

// 鉴权前两道 strict IP 桶的 Redis key。
// 前缀由 octo-lib 的 StrictIPRateLimitMiddleware 固定为 "ratelimit:strict:{tag}:"。
func heartbeatIPBucketKey(ip string) string { return "ratelimit:strict:bot_heartbeat:" + ip }
func registerIPBucketKey(ip string) string  { return "ratelimit:strict:bot_register:" + ip }

// unknownIP 是 lib 的 getClientIP 在拿不到客户端 IP 时的**失败闭合**取值。
// httptest 构造的请求没有 RemoteAddr，也没设 X-Real-Ip，故 register 的桶落在这个 key 上。
const unknownIP = "__unknown_ip__"

// drainIPBucket 把某个 strict 桶预先抽干，使随后一小段时间内的请求必然被拒。
//
// 用预抽干而不是"连打几百次"：靠真实请求打满既慢（每次都要走 DB 查询）又不稳定
// （打的过程中 token 一直在回填）。更重要的是,**预抽干让测试不依赖具体配额值**——
// 曾经有一条用例写死"打 200 次总能撞上限",默认 burst 从 50 调到 500 后它就变红了,
// 而被测行为一点没变。
//
// **`ts` 必须写到未来,不能写"现在"。** 令牌桶（本仓与 octo-lib 同一份 Lua）算的是
// `delta = math.max(0, now - ts)`、`filled = min(burst, tokens + delta*rate)`,
// 所以 tokens=0 且 ts=now 时,抽干与真正发请求之间流逝的那几毫秒就会回填出令牌:
// 500 rps 下只需 **2ms** 就有 1 个,而中间还夹着 Redis/DB 往返。第一版正是这么写的,
// 于是速率从 100 提到 500 rps 后这条用例开始间歇性变红——**一个按毫秒计时的竞态,
// 表现为"偶尔不限流"**。把 ts 推到未来则 delta 恒为 0,与配置的速率彻底无关。
//
// 桶的存储形状是 HMGET/HMSET 的 {tokens, ts}，见 pkg/ratelimit 的 Lua。
func drainIPBucket(t *testing.T, ctx *config.Context, key string) {
	t.Helper()
	c := rd.NewClient(&rd.Options{
		Addr:     ctx.GetConfig().DB.RedisAddr,
		Password: ctx.GetConfig().DB.RedisPass,
	})
	defer c.Close()
	// +5s 的余量:覆盖抽干到断言之间的全部往返(含 authBot 的 DB 查询),
	// 又远短于桶的 TTL,不会泄漏到后续用例。
	future := float64(time.Now().Add(5*time.Second).UnixNano()) / float64(time.Second)
	require.NoError(t, c.HMSet(key, map[string]interface{}{
		"tokens": 0,
		"ts":     future,
	}).Err())
	require.NoError(t, c.Expire(key, time.Minute).Err())
}

func heartbeatFrom(t *testing.T, h http.Handler, token, ip string) *httptest.ResponseRecorder {
	t.Helper()
	return botRequestFrom(t, h, "POST", "/v1/bot/heartbeat", token, ip)
}

// botRequestFrom 发一个带指定方法与 client IP 的请求。
// 方法可变是为了覆盖非 POST 的豁免路径（见 TestExcludedPathsAreLimitedOnRoutedNonPOSTMethods）。
func botRequestFrom(t *testing.T, h http.Handler, method, path, token, ip string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, err := http.NewRequest(method, path, nil)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	// getClientIP 优先读 X-Real-Ip,故此头决定桶的 key。
	req.Header.Set("X-Real-Ip", ip)
	h.ServeHTTP(w, req)
	return w
}

// TestExcludedPathsAreLimitedOnRoutedNonPOSTMethods —— 第二轮 code review P1-2 的行为回归。
//
// octo-lib 的全局限流按 `excludeSet[c.Request.URL.Path]` 匹配——**只看路径,不看方法**。
// 而两道 IP 底线原先只挂在 `r.POST` 上。于是 `GET /v1/bot/heartbeat`:
// 跳过全局桶（路径已豁免）→ 没有该方法的路由 → 落 gin no-route,全局中间件照跑
// （含访问日志）→ 渲染 404。**净效果是任意未鉴权来源可以无界地打 404**,
// 而这两条路径是全站唯一不受 1500 rps/IP 约束的地方（其它路径连不存在的 URL 都走全局桶）。
//
// 修法是改用 `r.Any` 注册、底线放最前、再由 onlyPOST 收口。这条用例钉住它:
// 非 POST 请求在桶耗尽后**必须**被限流,且 429 来自 strict IP 层。
//
// 此前所有 heartbeat/register 用例都是 POST,所以没有任何测试能发现这个洞。
//
// **本用例证明的范围仅限 gin 路由得到的方法**（这里取 GET/PUT/DELETE 作代表）。
// 用例名此前写作 `...OnEveryMethod`,而 `r.Any` 只覆盖 gin 的九个方法,故改名——
// 一个声称比它实际证明的更强的用例名,比没有用例更坏。
// 已知不覆盖、也确实仍绕过底线的两类（均非本改动引入,详见 ratelimit.go onlyPOST）:
//   - 扩展方法 `PROPFIND`/`LOCK`/`FOO` —— 匹配不到路由,落无底线的 no-route 404;
//   - `OPTIONS` —— 被 CORSMiddleware 在全局限流之前答 204,全站既有行为。
func TestExcludedPathsAreLimitedOnRoutedNonPOSTMethods(t *testing.T) {
	h, ctx := setupRateLimitTest(t)
	useParams(RateLimitParams{}) // per-bot 全关:复现生产默认姿态

	cases := []struct {
		name, method, path, scope string
		bucketKey                 func(ip string) string
	}{
		{"heartbeat/GET", "GET", "/v1/bot/heartbeat", "strict:bot_heartbeat", heartbeatIPBucketKey},
		{"heartbeat/DELETE", "DELETE", "/v1/bot/heartbeat", "strict:bot_heartbeat", heartbeatIPBucketKey},
		{"register/GET", "GET", "/v1/bot/register", "strict:bot_register", registerIPBucketKey},
		{"register/PUT", "PUT", "/v1/bot/register", "strict:bot_register", registerIPBucketKey},
	}

	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 每个子用例用独立 IP，避免互相消耗令牌。
			ip := fmt.Sprintf("198.51.100.%d", 10+i)

			// 对照:桶未耗尽时不应是 429。非 POST 会被 onlyPOST 收口成 404——
			// 这一条同时确认**状态码**与改动前一致（那时是 gin 的 404）；
			// 响应体从 gin 纯文本变成了 i18n 信封，此处不断言 body。
			first := botRequestFrom(t, h, c.method, c.path, "", ip)
			require.NotEqual(t, http.StatusTooManyRequests, first.Code,
				"桶未耗尽时不应限流——否则是误伤")
			require.Equal(t, http.StatusNotFound, first.Code,
				"非 POST 应以 404 结束（状态码与改动前一致），实际 %d", first.Code)

			drainIPBucket(t, ctx, c.bucketKey(ip))

			limited := botRequestFrom(t, h, c.method, c.path, "", ip)
			require.Equal(t, http.StatusTooManyRequests, limited.Code,
				"%s %s 在 IP 桶耗尽后仍未被限流——底线只挂在 POST 上,"+
					"非 POST 既跳过全局桶(豁免只看路径)又碰不到底线,"+
					"于是任意未鉴权来源可以无界打 404", c.method, c.path)
			require.Equal(t, c.scope, limited.Header().Get("X-RateLimit-Scope"),
				"429 应来自 strict IP 层")
		})
	}
}

// TestHeartbeatUnauthenticatedFloodIsRateLimited —— code review P1 的行为回归。
//
// `/v1/bot/heartbeat` 已从全局 per-IP 桶排除,那移走了它唯一的**未鉴权层**防护。
// per-bot 桶挂在 authBot 之后,而无效 token 在 authBot 里就 abort 了——永远走不到
// 那道桶,且它默认还是关闭的。缺少 authBot 之前的那道 strict IP 桶,攻击者就能用
// 任意无效 token 无限触发 authBot 的 Redis/DB 查询。
//
// 关键:本用例用**无效 token**。此前的 TestHeartbeatBucketStillEnforcesLimit 用的是
// 有效 token,只覆盖了鉴权之后那一半——它在这个漏洞存在时照样通过,是个假绿。
func TestHeartbeatUnauthenticatedFloodIsRateLimited(t *testing.T) {
	h, ctx := setupRateLimitTest(t)
	// per-bot 桶全关,复现生产默认姿态:此时唯一的防护就是鉴权前的 IP 桶。
	// IP 层由 lib 中间件承担，不受 provider 控制（无 kill switch），故这里全关即可。
	useParams(RateLimitParams{})

	const attackerIP = "203.0.113.7"

	// 对照:桶未耗尽时,无效 token 应当走到 authBot 并被鉴权拒绝(而非 429),
	// 证明 IP 桶没有误伤正常流量。
	first := heartbeatFrom(t, h, rlUnknownTk, attackerIP)
	require.NotEqual(t, http.StatusTooManyRequests, first.Code,
		"桶未耗尽时不应限流——否则是误伤")

	drainIPBucket(t, ctx, heartbeatIPBucketKey(attackerIP))

	limited := heartbeatFrom(t, h, rlUnknownTk, attackerIP)
	require.Equal(t, http.StatusTooManyRequests, limited.Code,
		"未鉴权的 heartbeat 洪水未被拦住——heartbeat 已移出全局 per-IP 桶，"+
			"若 authBot 之前没有 strict IP 桶，无效 token 可无限触发鉴权层的 Redis/DB 查询")
	require.Equal(t, "strict:bot_heartbeat", limited.Header().Get("X-RateLimit-Scope"),
		"429 应来自 authBot 之前的 strict IP 层（lib 中间件写的 scope）")

	// 另一个 IP 不受影响:这道桶是 per-IP 的,不是全局开关。
	other := heartbeatFrom(t, h, rlUnknownTk, "198.51.100.23")
	require.NotEqual(t, http.StatusTooManyRequests, other.Code,
		"其它 IP 被连坐了——strict 桶的维度不是 IP")
}
