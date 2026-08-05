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
