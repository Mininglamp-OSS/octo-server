package common

import (
	"math"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bot 限流配置的读侧防御测试(issue #696)。
//
// 无基础设施:直接塞 snapshot,不连 DB —— 同 stickerSnapSettings 的模式。
func botRLSettings(snap map[string]string) *SystemSettings {
	s := &SystemSettings{Log: log.NewTLog("SystemSettingsTest")}
	m := map[string]string{}
	for k, v := range snap {
		m[k] = v
	}
	s.snapshot.Store(&m)
	return s
}

// TestBotRateLimitDefaultsAreSafe —— 默认必须是「全关 + 影子」。
//
// 这条不是保守而是必需:register 层误配的后果是**全部 bot 无法注册**,
// 而合并一个默认开启的限流层意味着上线瞬间就在生产上做未经验证的拦截。
// 默认全关 ⇒ 合并后行为与改动前逐字节一致,开启完全靠翻开关。
func TestBotRateLimitDefaultsAreSafe(t *testing.T) {
	s := botRLSettings(nil)

	for name, enabled := range map[string]bool{
		"business":  s.BotRateLimitBusinessEnabled(),
		"heartbeat": s.BotRateLimitHeartbeatEnabled(),
		"register":  s.BotRateLimitRegisterEnabled(),
	} {
		assert.False(t, enabled, "%s 通道默认必须关闭", name)
	}
	for name, dryRun := range map[string]bool{
		"business":  s.BotRateLimitBusinessDryRun(),
		"heartbeat": s.BotRateLimitHeartbeatDryRun(),
		"register":  s.BotRateLimitRegisterDryRun(),
	} {
		assert.True(t, dryRun, "%s 通道默认必须是影子模式", name)
	}
}

// TestBotRateLimitHeartbeatRPSLowerBound —— heartbeat 速率的**结构性下界**。
//
// `bot:heartbeat:{robotID}` 的 TTL 是 60s(modules/bot_api heartbeatTTL)。若心跳配额
// 低到"一次被限流就让 key 过期",这条**保命**通道就自己变成了断联的成因——
// 正是 issue #696 要消除的那个故障。所以下界不是防手滑,是防止方案反向生效。
func TestBotRateLimitHeartbeatRPSLowerBound(t *testing.T) {
	// 常量关系:下界必须显著高于 1/TTL。60 是 bot_api.heartbeatTTL,
	// 跨包无法直接引用,故此处硬编码并由这条断言锁住语义。
	require.Greater(t, botRateLimitMinHeartbeatRPS, 1.0/60.0,
		"下界必须高于 1/heartbeatTTL,否则一次限流就可能让心跳 key 过期")

	cases := []struct {
		name string
		dbV  string
		want float64
	}{
		{"未设时用默认", "", defaultBotRateLimitHeartbeatRPS},
		{"低于下界被夹紧", "0.01", botRateLimitMinHeartbeatRPS},
		{"恰好下界保留", "0.1", botRateLimitMinHeartbeatRPS},
		{"高于下界原样通过", "5", 5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			snap := map[string]string{}
			if c.dbV != "" {
				snap["botratelimit.heartbeat_rps"] = c.dbV
			}
			assert.Equal(t, c.want, botRLSettings(snap).BotRateLimitHeartbeatRPS())
		})
	}
}

// TestBotRateLimitRegisterBurstFillRatioUpperBound —— register 的 `burst/rps` 上界。
//
// 第三轮 review P2-5。令牌桶 key 的 TTL 不是常数,而是
// `pkg/ratelimit/bucket.go` 的 Lua 从参数推的:`ttl = ceil(burst/rate * 2)`。
// 于是**每 IP 的 live key 上界 = 建 key 速率 × TTL**。register 的 key 是
// `SHA-256(客户端提供的 token)`,轮换 token 一请求一 key,基数只受 IP 底线约束,
// 所以 live key 随 TTL 线性发散;business/heartbeat 的 key 是 robotID,基数被
// 真实 bot 数封住,不需要这道夹紧。
//
// 关键是**方向反直觉**:TTL ∝ burst/rps,而上线流程是"按影子数据收紧 register_rps"。
// 收紧配额会放大 keyspace 上界。这条用例就是钉住"收紧不会放大"。
func TestBotRateLimitRegisterBurstFillRatioUpperBound(t *testing.T) {
	// 自证前提:出厂默认必须恰好落在界上,否则下面的 "≈4000 key" 论证失去锚点,
	// 而 bot_api 的底线注释是照这个数写的。
	require.Equal(t, botRateLimitMaxRegisterFillRatio,
		float64(defaultBotRateLimitRegisterBurst)/defaultBotRateLimitRegisterRPS,
		"出厂默认的 burst/rps 必须等于上限比值——register 底线注释里的 ≈4000 key 依赖它")

	cases := []struct {
		name           string
		dbRPS, dbBurst string
		want           int
	}{
		{"默认原样通过(恰在界上)", "", "", defaultBotRateLimitRegisterBurst},
		{"超界被夹到 rps*比值", "0.5", "500", 10},
		{"收紧 rps 会同时压低有效 burst", "0.25", "10", 5},
		{"放宽 rps 则允许更大 burst", "2", "40", 40},
		{"未超界原样通过", "1", "5", 5},
		{"rps 被下界夹紧后 burst 随之为 1", "0.001", "10", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			snap := map[string]string{}
			if c.dbRPS != "" {
				snap["botratelimit.register_rps"] = c.dbRPS
			}
			if c.dbBurst != "" {
				snap["botratelimit.register_burst"] = c.dbBurst
			}
			got := botRLSettings(snap).BotRateLimitRegisterBurst()
			assert.Equal(t, c.want, got)
			assert.GreaterOrEqual(t, got, 1,
				"burst 必须 >= 1,否则桶永远填不进 token,等于 100%% 拒绝")
		})
	}
}

// TestBotRateLimitRegisterKeyspaceBoundHoldsUnderTightening —— 上面那条夹紧的**目的**。
//
// 单独立一条,是因为 burst 的具体取值不是重点,「TTL 不随收紧而增长」才是。
// 这条直接复算 Lua 的 TTL 公式并断言 live key 上界不超过设计值,
// 所以将来有人放宽比值上界时,失败信息会直接指向真正被破坏的性质。
func TestBotRateLimitRegisterKeyspaceBoundHoldsUnderTightening(t *testing.T) {
	// bot_register IP 底线的 rps(modules/bot_api defaultRegisterIPRPS)。
	// 跨包不可引用,硬编码并由本断言锁住语义。
	const registerIPFloorRPS = 100.0
	const designedKeyBound = 4000.0

	// 自证前提:两道夹紧成对才守得住。下界必须恰好是"burst=1 时比值仍成立"的临界点,
	// 否则下面的上界断言会在低 rps 处失效——这个洞正是它抓出来的。
	require.Equal(t, 1.0, botRateLimitMinRegisterRPS*botRateLimitMaxRegisterFillRatio,
		"rps 下界必须等于 1/比值,否则 burst>=1 与比值约束冲突,TTL 会挣脱上界")

	for _, rps := range []string{"0.5", "0.25", "0.1", "0.05", "0.01", "0.001"} {
		t.Run("rps="+rps, func(t *testing.T) {
			s := botRLSettings(map[string]string{
				"botratelimit.register_rps": rps,
				// 故意给一个远超界的 burst:运维不会主动同步调它。
				"botratelimit.register_burst": "10000",
			})
			effRPS := s.BotRateLimitRegisterRPS()
			effBurst := s.BotRateLimitRegisterBurst()

			// 复算 pkg/ratelimit/bucket.go 的 Lua:ttl = max(1, ceil(burst/rate * 2))
			ttl := math.Max(1, math.Ceil(float64(effBurst)/effRPS*2))
			keyBound := registerIPFloorRPS * ttl

			assert.LessOrEqual(t, keyBound, designedKeyBound,
				"rps=%s: 有效 burst=%d ⇒ TTL=%.0fs ⇒ 每 IP 约 %.0f 个 live key,"+
					"超过设计上界 %.0f。收紧配额不得放大 keyspace",
				rps, effBurst, ttl, keyBound, designedKeyBound)
		})
	}
}

// TestBotRateLimitReadSideDefenseRejectsIllegalValues —— 读侧防御。
//
// 每条非法输入都对应一个真实失败模式:
//   - ≤0 会让令牌桶 Lua 走 `rate <= 0` 短路 ⇒ 整条路由 **100% 拒绝**
//     (注意:不是"关闭限流",是把路由打死);
//   - NaN 会让 Lua 算术全变 NaN、比较失效 ⇒ 行为不可预测。
//
// 两者都必须回退到**合法且仍然限流**的默认值,绝不能退化成放行——
// 静默 fail-open 会让一个配置错误伪装成"限流正常但没人超限"。
func TestBotRateLimitReadSideDefenseRejectsIllegalValues(t *testing.T) {
	for _, bad := range []string{"0", "-1", "-0.5", "NaN", "+Inf", "-Inf", "abc"} {
		t.Run("rps="+bad, func(t *testing.T) {
			got := botRLSettings(map[string]string{"botratelimit.business_rps": bad}).BotRateLimitBusinessRPS()
			assert.Equal(t, defaultBotRateLimitBusinessRPS, got, "非法 rps %q 必须回退到默认值", bad)
			assert.False(t, math.IsNaN(got) || math.IsInf(got, 0))
			assert.Greater(t, got, 0.0, "回退值本身必须合法,否则读侧防御就没有底")
		})
	}
	for _, bad := range []string{"0", "-1", "abc"} {
		t.Run("burst="+bad, func(t *testing.T) {
			got := botRLSettings(map[string]string{"botratelimit.business_burst": bad}).BotRateLimitBusinessBurst()
			assert.Equal(t, defaultBotRateLimitBusinessBurst, got, "非法 burst %q 必须回退到默认值", bad)
			assert.Greater(t, got, 0)
		})
	}
}

// TestBotRateLimitEnvFallbackIsSanitized —— **env 兜底本身也可能非有限**。
//
// `wkhttp.ParseRPSFromEnv` 底层是 `strconv.ParseFloat`,它会原样接受 "NaN" 和 "+Inf"。
// 所以"DB 未设 → 回退 env"这条路径上,env 的值必须先消毒再参与回退,
// 否则 NaN 会绕过 DB 侧的全部校验直达令牌桶。
// brief Acceptance 明确点名要覆盖这一条。
func TestBotRateLimitEnvFallbackIsSanitized(t *testing.T) {
	for _, bad := range []string{"NaN", "+Inf", "0", "-3"} {
		t.Run("env="+bad, func(t *testing.T) {
			t.Setenv(envBotRateLimitBusinessRPS, bad)
			got := botRLSettings(nil).BotRateLimitBusinessRPS() // DB 未设 ⇒ 走 env
			assert.Equal(t, defaultBotRateLimitBusinessRPS, got,
				"非有限/非正的 env 值 %q 必须回退到 code default,不得穿透到令牌桶", bad)
		})
	}

	// 对照:合法 env 必须生效,否则这层防御就成了"永远忽略 env"。
	t.Run("合法 env 生效", func(t *testing.T) {
		t.Setenv(envBotRateLimitBusinessRPS, "33.5")
		assert.Equal(t, 33.5, botRLSettings(nil).BotRateLimitBusinessRPS())
	})
}

// TestBotRateLimitDBOverridesEnv 钉住 DB → env → default 的优先级。
func TestBotRateLimitDBOverridesEnv(t *testing.T) {
	t.Setenv(envBotRateLimitRegisterRPS, "7")
	s := botRLSettings(map[string]string{"botratelimit.register_rps": "2.5"})
	assert.Equal(t, 2.5, s.BotRateLimitRegisterRPS(), "DB 必须压制 env")
}
