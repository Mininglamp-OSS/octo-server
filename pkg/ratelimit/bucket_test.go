package ratelimit

import (
	"os"
	"testing"
	"time"

	rd "github.com/go-redis/redis"
	"github.com/stretchr/testify/require"
)

// 本文件覆盖 bucket.allow 的两条**降级**路径。它们此前完全没有测试，而这恰恰是
// 最危险的空白：两条路径都 fail-open（放行），所以一旦它们在不该触发的时候触发，
// 症状是"限流静默失效"——线上表现为"限流装了但从未拒绝过任何请求"，
// 没有任何报错，只能靠 degraded 指标发现。
//
// code review 指出：`tokenBucketScript` 未来若被改动而返回形状变了，
// 没有任何测试会失败。这两个用例就是为此存在。

// unreachableClient 指向一个必然连不上的地址（端口 1 是保留端口）。
// DialTimeout 压到很小，避免用例把时间花在等超时上。
func unreachableClient() *rd.Client {
	return rd.NewClient(&rd.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 50 * time.Millisecond,
		MaxRetries:  0,
	})
}

// TestBucketAllowFailsOpenOnRedisError —— Redis 不可达时必须放行且标记 degraded。
//
// fail-open 是刻意的：Redis 是 IM 的核心依赖，它挂的时候整个系统已经失能；
// 若限流层此时反而开始拒绝，会把基础设施抖动放大成业务不可用。
//
// 但 degraded **必须**为 true：它是"限流实际上没在工作"的唯一信号，
// 若并入 allowed，运维在监控上就无法区分"没人超限"和"限流器瘫了"。
func TestBucketAllowFailsOpenOnRedisError(t *testing.T) {
	c := unreachableClient()
	defer c.Close()

	b := newBucket(c, "test:degraded:")
	res := b.allow("k1", 1, 1)

	require.True(t, res.allowed, "Redis 故障必须 fail-open，否则基础设施抖动会变成业务不可用")
	require.True(t, res.degraded, "Redis 故障必须标记 degraded，否则限流瘫了没人知道")
	require.Equal(t, 1, res.remaining, "degraded 时 remaining 回报 burst，避免下游误算余量")
}

// TestBucketAllowFailsOpenOnMalformedScriptResult —— 脚本返回形状异常时的行为。
//
// 用一个返回错误形状的脚本替换真脚本，模拟"将来有人改了 tokenBucketScript 的返回值"。
// 期望：放行 + degraded，而**不是** panic（未受检的类型断言）或静默当成拒绝。
//
// 三种形状都要覆盖：元素太少、元素太多、元素类型不对。前两者靠长度校验拦，
// 第三种靠带 ok 的类型断言兜——若哪天有人把 `arr[0].(int64)` 写成裸断言，
// 一个返回字符串的脚本就会让整个进程 panic。
func TestBucketAllowFailsOpenOnMalformedScriptResult(t *testing.T) {
	c := rd.NewClient(&rd.Options{Addr: testRedisAddr(), DialTimeout: 300 * time.Millisecond})
	defer c.Close()
	if err := c.Ping().Err(); err != nil {
		t.Skipf("需要本地 Redis：%v", err)
	}

	cases := []struct {
		name   string
		script string
		// wantDegraded 为 false 的用例说明形状合法、只是值异常，
		// 此时不应报 degraded（那会污染"限流瘫了"这个信号）。
		wantDegraded bool
		wantAllowed  bool
	}{
		{"元素太少", `return {1}`, true, true},
		{"元素太多", `return {1, 2, 3, 4}`, true, true},
		{"元素是字符串", `return {"yes", "no", "later"}`, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newBucket(c, "test:malformed:")
			b.script = rd.NewScript(tc.script)

			var res bucketResult
			require.NotPanics(t, func() { res = b.allow("k", 1, 5) },
				"脚本返回形状异常不得让进程 panic")

			require.Equal(t, tc.wantDegraded, res.degraded)
			require.Equal(t, tc.wantAllowed, res.allowed)
		})
	}
}

// TestBucketAllowClampsRemaining —— remaining 必须被夹到 [0, burst]。
// 它直接进 X-RateLimit-Remaining 响应头，越界值会让客户端的退避逻辑算错。
func TestBucketAllowClampsRemaining(t *testing.T) {
	c := rd.NewClient(&rd.Options{Addr: testRedisAddr(), DialTimeout: 300 * time.Millisecond})
	defer c.Close()
	if err := c.Ping().Err(); err != nil {
		t.Skipf("需要本地 Redis：%v", err)
	}

	for _, tc := range []struct {
		name   string
		script string
		burst  int
		want   int
	}{
		{"负数夹到 0", `return {1, -5, 0}`, 10, 0},
		{"超过 burst 夹到 burst", `return {1, 999, 0}`, 10, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBucket(c, "test:clamp:")
			b.script = rd.NewScript(tc.script)
			require.Equal(t, tc.want, b.allow("k", 1, tc.burst).remaining)
		})
	}
}

// TestTokenBucketScriptRejectsNonPositiveRate 钉住 Lua 的 `rate <= 0` 短路。
//
// 这条分支的后果容易被想反：它返回 {0,0,1} 即**拒绝**，不是放行。
// 所以配置里出现 rps<=0 时会把整条路由打死，而不是"关掉限流"——
// 这正是 sanitizeRPS 必须回退到合法默认值、而不能"当作未启用"的原因。
func TestTokenBucketScriptRejectsNonPositiveRate(t *testing.T) {
	c := rd.NewClient(&rd.Options{Addr: testRedisAddr(), DialTimeout: 300 * time.Millisecond})
	defer c.Close()
	if err := c.Ping().Err(); err != nil {
		t.Skipf("需要本地 Redis：%v", err)
	}

	b := newBucket(c, "test:zerorate:")
	res := b.allow("k", 0, 10)

	require.False(t, res.allowed, "rate<=0 在 Lua 里是拒绝而非放行——这就是必须做读侧防御的原因")
	require.False(t, res.degraded, "这是脚本的正常返回，不是降级")
	require.Equal(t, 1, res.retryAfter)
}

// testRedisAddr 默认与 octo-lib testutil.NewTestServer 使用同一个地址，
// 使本包测试与集成测试对环境的要求一致。
//
// 可用 `OCTO_TEST_REDIS_ADDR` 覆盖：同一台机器上并行跑测试的两个会话会争抢同一个
// Redis——本包的用例要清 `ratelimit:*` 前缀，跨会话互相清桶会产生"限流时好时坏"
// 这种极难归因的 flaky。写死端口的测试助手正是这类冲突的成因，所以留一个出口。
func testRedisAddr() string {
	if addr := os.Getenv("OCTO_TEST_REDIS_ADDR"); addr != "" {
		return addr
	}
	return "127.0.0.1:6379"
}
