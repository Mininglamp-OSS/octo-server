package bot_api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本文件是 issue #696 的**源码守卫**。这些性质靠人 review 记不住，必须由测试钉死。

// TestBotAPIMainGroupDoesNotReadLoginUID 是本次改动最危险的一处的守卫。
//
// 背景：`/v1/bot` 主组现在挂了 botActorUID()，它做的是 c.Set("uid", robotID)，
// 而 octo-lib 的 Context.GetLoginUID() 正是读 c.Get("uid")。
// 于是主组内任何调用 GetLoginUID() 的 handler 都会拿到 **robotID 而不是登录用户**。
//
// 实现时已逐个核实：包内 7 处 GetLoginUID() 全部在 oboCreateGrant / oboListGrants /
// oboDeleteGrant / oboUpdateGrant / oboCreateScope / oboDeleteScope / oboListScopes，
// 它们挂在 /v1/obo/* user-token 组，不在主组；主组内唯一的 obo handler
// oboBotGetGrant 不读 uid。**当前是安全的。**
//
// 但那是一条"今天为真"的性质，不是结构性保证：将来任何人往主组加一个读
// GetLoginUID() 的 handler，就会静默地把 bot 身份当成登录用户——那是一个鉴权
// 语义错误，不会有编译错误、大概率也不会有测试失败。这个守卫就是为此存在。
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
			"主组挂了 botActorUID()，GetLoginUID() 在这里返回的是 robotID 而非登录用户。"+
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
	require.Contains(t, src, `r.POST("/v1/bot/heartbeat"`,
		"heartbeat 应在主组之外独立注册并挂自己的限流通道")
}

// TestRateLimitMountOrder 钉住挂载顺序。
//
// 顺序必须是 authBot → botActorUID → 限流：per-bot 限流靠 botActorUID 写入的身份
// 取维度，顺序颠倒会让限流在拿不到 key 时**静默旁路**（fail-open），
// 即"限流看起来装上了但从未生效"——这种失效没有任何报错，只能靠守卫拦。
func TestRateLimitMountOrder(t *testing.T) {
	src := readBotAPISource(t)

	group := extractMainGroupDecl(t, src)
	authIdx := strings.Index(group, "ba.authBot()")
	actorIdx := strings.Index(group, "ba.botActorUID()")
	limitIdx := strings.Index(group, "ba.rateLimitMiddleware(")

	require.NotEqual(t, -1, authIdx, "主组必须挂 authBot")
	require.NotEqual(t, -1, actorIdx, "主组必须挂 botActorUID")
	require.NotEqual(t, -1, limitIdx, "主组必须挂 per-bot 限流")

	require.Less(t, authIdx, actorIdx, "botActorUID 必须在 authBot 之后")
	require.Less(t, actorIdx, limitIdx, "限流必须在 botActorUID 之后，否则取不到 bot 身份、静默旁路")
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

	start := strings.Index(src, `r.POST("/v1/bot/heartbeat"`)
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
	rStart := strings.Index(src, `r.POST("/v1/bot/register"`)
	require.NotEqual(t, -1, rStart)
	rDecl := src[rStart:]
	if end := strings.Index(rDecl, "ba.register)"); end != -1 {
		rDecl = rDecl[:end]
	}
	require.Less(t, strings.Index(rDecl, "registerIPLimit"), strings.Index(rDecl, "l.register }"),
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
