package ratelimit

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCheckBypassesWithoutTouchingRedis 钉住 kill switch 的**完全旁路**语义。
//
// 这里刻意用 nil Redis client：若 Enabled=false 时仍然去查桶，nil 解引用会 panic，
// 测试立刻变红。用「不 panic」证明「没查 Redis」比断言调用次数更难被后续重构绕过。
//
// 为什么这条重要：kill switch 是误配时的止损手段，它自己必须零依赖——
// 如果关闭限流反而还要求 Redis 可用，那在 Redis 故障时就没法止损了。
func TestCheckBypassesWithoutTouchingRedis(t *testing.T) {
	l := New(nil, "test:", "business", func() Params {
		return Params{Enabled: false, RPS: 10, Burst: 10}
	}, nil, Params{RPS: 1, Burst: 1})

	res := l.Check("bot-1")

	require.Equal(t, OutcomeBypassed, res.Outcome)
	require.False(t, res.ShouldReject(), "旁路不得拒绝")
	require.False(t, res.ShouldSetHeaders(), "旁路不得下发 X-RateLimit-* 头")
}

// TestCheckBypassesOnEmptyKey：拿不到身份就限不了流，此时 fail-open。
// 同样用 nil client 证明未查 Redis。
func TestCheckBypassesOnEmptyKey(t *testing.T) {
	l := New(nil, "test:", "business", func() Params {
		return Params{Enabled: true, RPS: 10, Burst: 10}
	}, nil, Params{RPS: 1, Burst: 1})

	require.Equal(t, OutcomeBypassed, l.Check("").Outcome)
}

// TestResultSemantics 钉住四种 outcome 与「是否拒绝 / 是否下发头」的映射。
//
// 载荷性的两条：
//   - WouldDeny 必须 ShouldReject()==false —— 影子模式判定为拒但不拦截；
//   - WouldDeny 必须 ShouldSetHeaders()==false —— 影子期若下发限流头，客户端会据此
//     自行降频，观测到的就不再是真实流量，而观测的全部意义就是回答「设成这个配额
//     会拒谁」。这条不是洁癖，是影子模式成立的前提。
func TestResultSemantics(t *testing.T) {
	cases := []struct {
		outcome     Outcome
		reject      bool
		setHeaders  bool
		labelString string
	}{
		{OutcomeBypassed, false, false, "bypassed"},
		{OutcomeAllowed, false, true, "allowed"},
		{OutcomeWouldDeny, false, false, "would_deny"},
		{OutcomeDenied, true, true, "denied"},
		{OutcomeDegraded, false, false, "degraded"},
	}
	for _, c := range cases {
		t.Run(c.labelString, func(t *testing.T) {
			r := Result{Outcome: c.outcome}
			require.Equal(t, c.reject, r.ShouldReject())
			require.Equal(t, c.setHeaders, r.ShouldSetHeaders())
			require.Equal(t, c.labelString, c.outcome.String())
		})
	}
}

// TestDryRunNeverSetsHeaders 钉住一个**实际发生过的缺陷**。
//
// 初版实现只按 Outcome 判断是否下发头，于是 dry-run 下**被放行**的请求
// （outcome=Allowed，因为判定结果确实是放行）照样下发了 X-RateLimit-*。
// 后果不是"多了个头"这么轻：影子期客户端看到 Remaining 递减就会自行降频，
// 于是观测到的不再是真实流量，而观测的唯一目的就是回答"设成这个配额会拒谁"。
//
// 集成测试 TestRateLimitDryRunDoesNotAffectClients 是发现它的地方；
// 这条单测把它钉在更便宜的层次上。
func TestDryRunNeverSetsHeaders(t *testing.T) {
	for _, o := range []Outcome{OutcomeAllowed, OutcomeWouldDeny, OutcomeDegraded} {
		t.Run(o.String(), func(t *testing.T) {
			r := Result{Outcome: o, DryRun: true}
			require.False(t, r.ShouldSetHeaders(), "影子模式下 %s 不得下发限流头", o)
			require.False(t, r.ShouldReject(), "影子模式下 %s 不得拦截", o)
		})
	}

	// 对照：非影子模式下 Allowed 是要下发头的，否则客户端拿不到配额余量。
	require.True(t, Result{Outcome: OutcomeAllowed}.ShouldSetHeaders())
}

// TestOutcomeStringsAreDistinct：would_deny 与 denied 必须是不同的 label 值，
// 否则「关掉 dry-run 会多拒多少」就无从计算，影子模式失去意义。
func TestOutcomeStringsAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, o := range []Outcome{OutcomeBypassed, OutcomeAllowed, OutcomeWouldDeny, OutcomeDenied, OutcomeDegraded} {
		s := o.String()
		require.False(t, seen[s], "outcome label 重复: %s", s)
		seen[s] = true
	}
	require.Len(t, seen, 5)
}

// TestSanitizeRPS 是**读侧防御**的回归钉。
//
// 每一条非法输入都对应一个真实的失败模式：
//   - <=0 会让令牌桶 Lua 走 `rate <= 0` 短路 → 整条路由 100% 拒绝（不是"关闭限流"）；
//   - NaN 会让 Lua 的算术全部变 NaN、比较失效 → 行为不可预测；
//   - +Inf 同理，且 burst/rate 会让 EXPIRE 拿到非法 TTL。
//
// NaN / ±Inf 不是臆想的输入：wkhttp.ParseRPSFromEnv 底层是 strconv.ParseFloat，
// 会原样接受 "NaN" 和 "+Inf"，所以 env 兜底本身就可能是非有限值。
func TestSanitizeRPS(t *testing.T) {
	const fallback = 7.5
	cases := []struct {
		name string
		in   float64
		want float64
	}{
		{"正常值原样通过", 20, 20},
		{"零回退", 0, fallback},
		{"负数回退", -1, fallback},
		{"NaN 回退", math.NaN(), fallback},
		{"正无穷回退", math.Inf(1), fallback},
		{"负无穷回退", math.Inf(-1), fallback},
		{"极小正数保留", 0.001, 0.001},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, sanitizeRPS(c.in, fallback))
		})
	}
}

// TestSanitizeBurst：burst<=0 会让桶永远填不进 1 个 token，同样等于 100% 拒绝。
func TestSanitizeBurst(t *testing.T) {
	const fallback = 42
	require.Equal(t, 100, sanitizeBurst(100, fallback))
	require.Equal(t, fallback, sanitizeBurst(0, fallback))
	require.Equal(t, fallback, sanitizeBurst(-5, fallback))
}

// TestObserverReceivesBypassed 确认旁路也会上报——运维需要能从指标上区分
// 「这层没启用」和「这层启用了但没人超限」，两者在业务表现上完全一样。
func TestObserverReceivesBypassed(t *testing.T) {
	var gotClass, gotKey string
	var gotOutcome Outcome
	obs := observerFunc(func(class, key string, o Outcome) {
		gotClass, gotKey, gotOutcome = class, key, o
	})

	l := New(nil, "test:", "heartbeat", func() Params { return Params{Enabled: false} }, obs, Params{RPS: 1, Burst: 1})
	l.Check("bot-9")

	require.Equal(t, "heartbeat", gotClass)
	require.Equal(t, "bot-9", gotKey)
	require.Equal(t, OutcomeBypassed, gotOutcome)
}

type observerFunc func(class, key string, o Outcome)

func (f observerFunc) Observe(class, key string, o Outcome) { f(class, key, o) }
