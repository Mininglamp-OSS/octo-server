package bot_api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/metrics"
	"github.com/Mininglamp-OSS/octo-server/pkg/ratelimit"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

// 本文件是 issue #696 的**源码守卫**。这些性质靠人 review 记不住，必须由测试钉死。

// TestBotAPIMainGroupDoesNotReadLoginUID 守卫「bot 身份不得被当成登录用户」。
//
// 背景（含一次自我修正）：初版往 `/v1/bot` 主组挂了 `botActorUID()`，它会
// `c.Set("uid", robotID)`，而 octo-lib 的 `Context.GetLoginUID()` 正是读 `c.Get("uid")`。
// 于是主组内任何调 `GetLoginUID()` 的 handler 都会拿到 **robotID 而不是登录用户**。
// 第二轮 code review 指出：per-bot 限流的维度取自 `"robot_id"`，**压根不需要那个别名**,
// 所以那个风险是白担的。现已换成 `requireBotIdentity()`——只做 fail-closed 的身份断言,
// 不写 `uid`。
//
// **守卫因此依然必要,而且理由更强了**:现在主组不再别名 `uid`,所以
//   - 若有人重新挂上 `botActorUID()`（或任何写 `uid` 的中间件），别名风险原样回来;
//   - 而只要主组内没有 handler 读 `GetLoginUID()`，那种混淆就无从发生。
//
// 这个守卫钉的是后者——它是那个风险的**结构性前提**，与具体挂了哪个中间件无关。
// 包内 7 处 `GetLoginUID()` 全在 oboCreateGrant / oboListGrants / oboDeleteGrant /
// oboUpdateGrant / oboCreateScope / oboDeleteScope / oboListScopes，它们挂在
// /v1/obo/* user-token 组；主组内唯一的 obo handler oboBotGetGrant 不读 uid。
//
// 将来任何人往主组加一个读 `GetLoginUID()` 的 handler，都不会有编译错误、
// 大概率也不会有别的测试失败——这个守卫就是为此存在。
func TestBotAPIMainGroupDoesNotReadLoginUID(t *testing.T) {
	handlers := mainGroupHandlerNames(t)
	// 自证：解析必须真的抓到了主组 handler。没有这两条，一旦 Route 的写法变化
	// 导致正则失配，守卫会安静地变成"空集合 vs 任意集合"的恒真断言——
	// 一个假绿的守卫比没有守卫更危险。
	require.NotEmpty(t, handlers, "解析不到主组 handler，守卫失效——先修守卫本身")
	require.Contains(t, handlers, "sendMessage", "主组解析结果不含已知 handler，正则已失配")

	readers := functionsCallingGetLoginUID(t)
	// 自证：AST 扫描必须真的能识别出 GetLoginUID 调用。
	// oboCreateGrant 是已知的读取方（挂在 /v1/obo/*，不在主组）。
	require.Contains(t, readers, "oboCreateGrant", "AST 扫描不到已知的 GetLoginUID 调用方，守卫失效")

	var offenders []string
	for name := range handlers {
		if readers[name] {
			offenders = append(offenders, name)
		}
	}

	require.Empty(t, offenders,
		"以下 handler 挂在 /v1/bot 主组且调用了 c.GetLoginUID()：%v\n"+
			"该组是 bot-token 组，`uid` 不代表登录用户；一旦有中间件把 robotID 写进 `uid`"+
			"（如 botActorUID），这些 handler 会静默地把 bot 当成登录用户——鉴权语义错误。"+
			"若确实需要登录用户身份，该端点不应挂在 bot-token 组下。", offenders)
}

// TestBotHeartbeatIsOutsideMainGroup 钉住心跳的独立配额。
//
// 心跳若留在主组，就会与业务端点共用 business 桶——那样"给心跳独立配额"这件事
// 就没有发生：一次 7-8 条消息的推理爆发照样能把心跳挤掉，而这正是 issue #696
// 报告的现象。
func TestBotHeartbeatIsOutsideMainGroup(t *testing.T) {
	handlers := mainGroupHandlerNames(t)
	require.NotContains(t, handlers, "heartbeat",
		"heartbeat 不得注册在 botAPI 主组内，否则它与业务端点共用同一个令牌桶，"+
			"独立配额形同虚设（issue #696）")

	src := readBotAPISource(t)
	require.Contains(t, src, `r.Any("/v1/bot/heartbeat"`,
		"heartbeat 应在主组之外独立注册并挂自己的限流通道")
}

// TestRateLimitMountOrder 钉住挂载顺序。
//
// 顺序必须是 authBot → requireBotIdentity → 限流。限流的维度取自 authBot 写的
// "robot_id"，顺序颠倒会让它拿不到 key 而**静默旁路**（OutcomeNoKey，fail-open），
// 即"限流看起来装上了但从未生效"——这种失效没有任何报错，只能靠守卫拦。
//
// requireBotIdentity 也必须在限流之前：它是 fail-closed 的身份断言，
// 排在后面就失去了「身份缺失时报错而非放行」的意义。
func TestRateLimitMountOrder(t *testing.T) {
	src := readBotAPISource(t)

	group := extractMainGroupDecl(t, src)
	authIdx := strings.Index(group, "ba.authBot()")
	identityIdx := strings.Index(group, "ba.requireBotIdentity()")
	limitIdx := strings.Index(group, "ba.rateLimitMiddleware(")

	require.NotEqual(t, -1, authIdx, "主组必须挂 authBot")
	require.NotEqual(t, -1, identityIdx, "主组必须挂 requireBotIdentity")
	require.NotEqual(t, -1, limitIdx, "主组必须挂 per-bot 限流")

	require.Less(t, authIdx, identityIdx, "requireBotIdentity 必须在 authBot 之后")
	require.Less(t, identityIdx, limitIdx, "限流必须在身份断言之后，否则取不到 bot 身份、静默旁路")

	// 主组**不得**挂 botActorUID:它会把 robotID 别名成 "uid",而限流并不需要那个别名。
	// 见 TestBotAPIMainGroupDoesNotReadLoginUID 的注释（第二轮 code review P2-2）。
	require.NotContains(t, group, "ba.botActorUID()",
		"主组不应挂 botActorUID——它写 c.Set(\"uid\", robotID)，会让任何调 GetLoginUID() 的 "+
			"handler 把 bot 当成登录用户，而 per-bot 限流用的是 \"robot_id\"，不需要这个别名")
}

// TestBotTokenFingerprintDoesNotLeakToken 钉住凭据不落明文。
//
// register 的限流维度是 token 指纹（该端点在 authBot 之前，没有 bot 身份可用）。
// 指纹会进 Redis key，因此绝不能包含 token 明文或其任何前缀。
func TestBotTokenFingerprintDoesNotLeakToken(t *testing.T) {
	const token = "bf_super_secret_token_value"

	fp := botTokenFingerprint(token)

	require.NotEmpty(t, fp)
	require.NotContains(t, fp, token)
	require.NotContains(t, fp, "bf_")
	require.NotContains(t, fp, "secret")
	require.Len(t, fp, 32, "SHA-256 前 16 字节的 hex")
	require.Regexp(t, regexp.MustCompile(`^[0-9a-f]+$`), fp)

	// 稳定性：同一 token 必须落到同一个桶，否则重试风暴会不断开新桶、限流失效。
	require.Equal(t, fp, botTokenFingerprint(token))
	// 区分度：不同 token 必须分桶。
	require.NotEqual(t, fp, botTokenFingerprint(token+"x"))
	// 空 token 返回空 key → Limiter 旁路（此时该拒绝的是 register handler 自己）。
	require.Equal(t, "", botTokenFingerprint(""))
}

// --- helpers ---

func readBotAPISource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("bot_api.go")
	require.NoError(t, err)
	return string(b)
}

// extractMainGroupDecl 截取 `botAPI := r.Group(...)` 这一条声明的文本。
func extractMainGroupDecl(t *testing.T, src string) string {
	t.Helper()
	start := strings.Index(src, "botAPI := r.Group(")
	require.NotEqual(t, -1, start, "找不到主组声明，守卫失效——先修守卫本身")
	rest := src[start:]
	end := strings.Index(rest, "\n\t)")
	require.NotEqual(t, -1, end, "主组声明格式变化，守卫失效——先修守卫本身")
	return rest[:end]
}

var mainGroupHandlerRe = regexp.MustCompile(`botAPI\.(?:GET|POST|PUT|DELETE|PATCH)\([^,]+,\s*ba\.(\w+)\)`)

// mainGroupHandlerNames 返回注册在 botAPI 主组内的 handler 方法名集合。
func mainGroupHandlerNames(t *testing.T) map[string]bool {
	t.Helper()
	src := readBotAPISource(t)
	out := map[string]bool{}
	for _, m := range mainGroupHandlerRe.FindAllStringSubmatch(src, -1) {
		out[m[1]] = true
	}
	return out
}

// functionsCallingGetLoginUID 用 AST 找出包内所有调用了 c.GetLoginUID() 的方法名。
// 用 AST 而非 grep：grep 会把注释、字符串里的同名文本算进来，守卫必须只认真实调用。
func functionsCallingGetLoginUID(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	require.NoError(t, err)

	out := map[string]bool{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				ast.Inspect(fn, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					if sel.Sel.Name == "GetLoginUID" {
						out[fn.Name.Name] = true
					}
					return true
				})
			}
		}
	}
	return out
}

// TestBotRateLimitMetricHelpMatchesActualEnums —— 指标 Help 必须与真实枚举一致。
//
// 这条守卫存在的理由本身就是证据：`class(4) × outcome(5)`、枚举里写一个不存在的
// `events`、漏掉后来新增的 `no_key`——同一处文档漂移被**三位评审提了五次**，
// 每次都靠人眼发现。文档与枚举的一致性是可判定的，就不该靠 review 记。
//
// 为什么钉在 bot_api 而不是 pkg/metrics：class 枚举在本包，outcome 枚举在
// pkg/ratelimit，而 pkg/metrics 不能 import 本包（bot_api → metrics 已成依赖）。
// 本包是两个枚举唯一的交汇点。
//
// 钉 Help 而不是注释：Help 是**下发到 Prometheus、被看板作者读到**的那份文档，
// 漂移的实际代价落在这里；而它可以从 registry 里取回来比对，注释不能。
func TestBotRateLimitMetricHelpMatchesActualEnums(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.NewBotRateLimitMetrics(reg)

	// CounterVec 在没有任何 label 组合被观测过时不会出现在 Gather 结果里，
	// 必须先造一个 series 才能取到 Help。
	m.Decisions.WithLabelValues(RateLimitClassBusiness, ratelimit.OutcomeAllowed.String()).Inc()

	families, err := reg.Gather()
	require.NoError(t, err)

	var help string
	for _, f := range families {
		if strings.HasSuffix(f.GetName(), "bot_ratelimit_decisions_total") {
			help = f.GetHelp()
		}
	}
	require.NotEmpty(t, help,
		"取不到 decisions_total 的 Help——指标改名了？本守卫会因此静默失效")

	// outcome:遍历真实枚举，Help 必须逐个提到。
	// 用 String() 而非字面量，这样新增 outcome 时本用例自动跟着变严。
	for _, o := range []ratelimit.Outcome{
		ratelimit.OutcomeBypassed, ratelimit.OutcomeAllowed, ratelimit.OutcomeWouldDeny,
		ratelimit.OutcomeDenied, ratelimit.OutcomeDegraded, ratelimit.OutcomeNoKey,
	} {
		require.Contains(t, help, o.String(),
			"Help 未提到 outcome %q。看板作者据 Help 建查询，漏一个就等于那类判定在监控上不存在", o.String())
	}

	// class:三个通道全部提到。
	for _, c := range []string{RateLimitClassBusiness, RateLimitClassHeartbeat, RateLimitClassRegister} {
		require.Contains(t, help, c, "Help 未提到 class %q", c)
	}

	// 反向:不得出现不存在的枚举值。`events` 是曾经真实写进 Help 的幽灵值,
	// 它会让看板作者为一个永不产生数据的 class 建查询。
	require.NotContains(t, help, "events",
		"Help 提到了不存在的 class `events`——没有任何代码会发出它")
	require.NotContains(t, help, "unknown",
		"`unknown` 是 Outcome.String() 的 default 兜底,不是合法枚举值,不应写进 Help")
}

// TestHeartbeatIPLimitPrecedesAuth 是 code review P1 的源码守卫。
//
// `/v1/bot/heartbeat` 已从全局 per-IP 桶排除（main.go globalRateLimitExcludePaths），
// 那也移走了它唯一的**未鉴权层**防护。per-bot 桶挂在 authBot 之后，而无效 token 在
// authBot 里就 abort 了——永远走不到那道桶。所以必须有一道 strict IP 桶排在
// authBot **之前**，否则攻击者可用任意无效 token 无限触发 authBot 的 Redis/DB 查询：
// 原本被全局桶挡住的 DDoS 面，反而因为 exclude 被打开。
//
// 这个顺序无法靠类型系统或编译期保证，且调换后所有既有测试仍然通过
// （它们都用有效 token，走的是鉴权之后那一半），故必须由守卫钉死。
func TestHeartbeatIPLimitPrecedesAuth(t *testing.T) {
	src := readBotAPISource(t)

	start := strings.Index(src, `r.Any("/v1/bot/heartbeat"`)
	require.NotEqual(t, -1, start, "找不到 heartbeat 路由声明，守卫失效——先修守卫本身")
	decl := src[start:]
	if end := strings.Index(decl, "ba.heartbeat)"); end != -1 {
		decl = decl[:end]
	}

	ipIdx := strings.Index(decl, "heartbeatIPLimit")
	authIdx := strings.Index(decl, "ba.authBot()")
	perBotIdx := strings.Index(decl, "l.heartbeat }")

	require.NotEqual(t, -1, ipIdx,
		"heartbeat 缺少 per-IP strict 限流。它已移出全局 per-IP 桶，"+
			"没有这一层则无效 token 可无限触发 authBot 的 Redis/DB 查询")
	require.NotEqual(t, -1, authIdx)
	require.NotEqual(t, -1, perBotIdx)

	require.Less(t, ipIdx, authIdx,
		"per-IP 限流必须在 authBot 之前——放在之后等于只保护已鉴权流量，"+
			"而未鉴权洪水正是 exclude 打开的那个面")
	require.Less(t, authIdx, perBotIdx,
		"per-bot 桶必须在 authBot 之后，否则取不到 bot 身份")

	// 参数必须过 ipLimitParams 消毒——ParseRPSFromEnv 会接受 "NaN"，而 lib 的
	// newKeyedLimiter 只挡 rps<=0（NaN<=0 为 false），于是 NaN 会穿过启动期校验
	// 直达 Lua、让所有算术与比较静默失效。
	require.Regexp(t,
		`heartbeatIPRPS,\s*heartbeatIPBurst\s*:=\s*ipLimitParams\(`,
		src, "heartbeat IP 参数必须过 ipLimitParams 消毒")
	require.Regexp(t,
		`registerIPRPS,\s*registerIPBurst\s*:=\s*ipLimitParams\(`,
		src, "register IP 参数同样必须过 ipLimitParams 消毒")

	// register 侧同样两层，且 IP 在前——否则随机 token 已经把 Redis key 建好了。
	rStart := strings.Index(src, `r.Any("/v1/bot/register"`)
	require.NotEqual(t, -1, rStart)
	rDecl := src[rStart:]
	if end := strings.Index(rDecl, "ba.register)"); end != -1 {
		rDecl = rDecl[:end]
	}

	// 先自证两端都在，再比顺序（第三轮 review P2-6）。
	// 原先直接 `require.Less(Index(a), Index(b))`：`registerIPLimit` 缺失时 Index 返回
	// -1，而 `Less(-1, 正数)` **通过**——这条守护 keyspace 放大顺序的断言在中间件被
	// 删掉时会静默空过。上面 heartbeat 块（:229）本来就是这么写的，这里漏了。
	// 这正是本任务反复强调的那类失效：守卫一旦匹配不上就退化成恒真断言。
	rIPIdx := strings.Index(rDecl, "registerIPLimit")
	rPerTokenIdx := strings.Index(rDecl, "l.register }")

	require.NotEqual(t, -1, rIPIdx,
		"register 缺少 per-IP strict 限流。它已移出全局 per-IP 桶且跑在鉴权之前，"+
			"没有这一层则轮换 token 可按请求速率无界地造 Redis key")
	require.NotEqual(t, -1, rPerTokenIdx,
		"register 缺少 per-token 指纹桶")

	require.Less(t, rIPIdx, rPerTokenIdx,
		"register 的 IP 桶必须排在 token 指纹桶之前，否则 key 已经建好了")
}

// TestIPLimitParamsSanitizesEnv 钉住 IP 层的读侧防御。
//
// 补的是 octo-lib 的一个真实缺口:`newKeyedLimiter` 构造时只检查 `rps <= 0` 就 panic,
// 但 **`NaN <= 0` 为 false**,所以 `OCTO_BOT_RATELIMIT_HEARTBEAT_IP_RPS=NaN` 会穿过
// 那道启动期校验直达令牌桶 Lua,让所有算术和比较失效——行为不可预测且无任何报错。
// `ParseRPSFromEnv` 底层是 `strconv.ParseFloat`,它确实会接受 "NaN"/"+Inf"。
func TestIPLimitParamsSanitizesEnv(t *testing.T) {
	const defRPS, defBurst = 100.0, 300

	t.Run("未设时用默认", func(t *testing.T) {
		rps, burst := ipLimitParams(envHeartbeatIPRPS, defRPS, envHeartbeatIPBurst, defBurst)
		require.Equal(t, defRPS, rps)
		require.Equal(t, defBurst, burst)
	})

	for _, bad := range []string{"NaN", "+Inf", "-Inf", "0", "-1", "abc"} {
		t.Run("非法 rps="+bad, func(t *testing.T) {
			t.Setenv(envHeartbeatIPRPS, bad)
			rps, _ := ipLimitParams(envHeartbeatIPRPS, defRPS, envHeartbeatIPBurst, defBurst)
			require.Equal(t, defRPS, rps,
				"非法 env %q 必须回退——否则 NaN 直达 Lua，或 rps<=0 让整条路由 100%% 拒绝", bad)
		})
	}

	for _, bad := range []string{"0", "-5", "abc"} {
		t.Run("非法 burst="+bad, func(t *testing.T) {
			t.Setenv(envHeartbeatIPBurst, bad)
			_, burst := ipLimitParams(envHeartbeatIPRPS, defRPS, envHeartbeatIPBurst, defBurst)
			require.Equal(t, defBurst, burst)
		})
	}

	// 对照:合法 env 必须生效,否则消毒会退化成"永远忽略 env",
	// 那么"改 configmap 就能调"这个前提也不成立。
	t.Run("合法 env 生效", func(t *testing.T) {
		t.Setenv(envHeartbeatIPRPS, "250")
		t.Setenv(envHeartbeatIPBurst, "800")
		rps, burst := ipLimitParams(envHeartbeatIPRPS, defRPS, envHeartbeatIPBurst, defBurst)
		require.Equal(t, 250.0, rps)
		require.Equal(t, 800, burst)
	})
}
