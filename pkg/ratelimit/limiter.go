package ratelimit

import (
	rd "github.com/go-redis/redis"
)

// Outcome 是一次 Check 的判定结果。
//
// WouldDeny 与 Denied 必须是**两个不同的值**：影子期（dry-run）与执行期的数据
// 若混在一起，就无法回答「关掉 dry-run 之后会多拒多少」——而那正是影子模式存在
// 的唯一理由。
type Outcome int

const (
	// OutcomeBypassed 表示该层未启用（enabled=false），完全旁路：
	// 没有查过 Redis，也不应设置任何 X-RateLimit-* 头。
	OutcomeBypassed Outcome = iota
	// OutcomeAllowed 表示桶判定放行。
	OutcomeAllowed
	// OutcomeWouldDeny 表示桶判定拒绝，但因 dry-run 而**未**拦截。
	OutcomeWouldDeny
	// OutcomeDenied 表示桶判定拒绝且已拦截。
	OutcomeDenied
	// OutcomeDegraded 表示 Redis 故障，fail-open 放行。
	OutcomeDegraded
)

// String 供指标 label 与日志使用。取值是**固定枚举**，这是 Prometheus label
// 基数有界的前提之一。
func (o Outcome) String() string {
	switch o {
	case OutcomeBypassed:
		return "bypassed"
	case OutcomeAllowed:
		return "allowed"
	case OutcomeWouldDeny:
		return "would_deny"
	case OutcomeDenied:
		return "denied"
	case OutcomeDegraded:
		return "degraded"
	default:
		return "unknown"
	}
}

// Params 是一次判定所用的运行时配置。由 ParamsFunc 在**每次判定时**返回，
// 因此改 system_setting 后最多一个配置快照周期（60s）即生效，无需重启。
type Params struct {
	// Enabled=false 时完全旁路：不查 Redis、不设响应头、不改变任何可观察行为。
	// 这是 kill switch，误伤时用它止损比回滚发版快一个数量级。
	Enabled bool
	// DryRun=true 时执行完整判定（含 Redis 往返，成本要真实暴露）并上报观测，
	// 但**不拦截**、**不设 X-RateLimit-* 头**。
	//
	// 「不设头」是硬要求而非洁癖：影子期若下发限流头，客户端可能据此自行降频，
	// 于是观测到的不再是真实流量，定值也就失去意义。
	DryRun bool
	RPS    float64
	Burst  int
}

// ParamsFunc 每次判定时被调用一次。实现方负责读配置并做读侧防御。
type ParamsFunc func() Params

// Observer 接收每次判定的结果。实现必须是**非阻塞**的：它跑在请求路径上。
//
// key 是限流维度的业务标识（本任务里是 robotID）。它**只允许**流向低基数的
// 聚合结构（如 Redis 的 top-N ZSet），**不得**作为 Prometheus label——生产实测
// 活跃 bot 2903 个且随业务增长无上界。
type Observer interface {
	Observe(class string, key string, outcome Outcome)
}

// Result 是 Check 返回给调用方的判定结论。是否拒绝、如何拒绝由调用方决定，
// 本包不依赖任何 HTTP 框架。
type Result struct {
	Outcome    Outcome
	Remaining  int
	RetryAfter int
	// Burst 回传当次判定实际使用的桶容量，供调用方写 X-RateLimit-Limit。
	// 不能让调用方自己再读一次配置——两次读之间配置可能已经变了。
	Burst int
	// DryRun 标记本次判定处于影子模式。
	//
	// 必须单独带出来，而不能从 Outcome 反推：影子模式下**放行**的请求 outcome 仍是
	// OutcomeAllowed（判定结果就是放行，这不是影子特有的），若只看 outcome 就会给它
	// 下发 X-RateLimit-* 头——于是影子期客户端照样看到 Remaining 递减并自行降频，
	// 观测到的不再是真实流量。集成测试
	// TestRateLimitDryRunDoesNotAffectClients 钉住这条。
	DryRun bool
}

// ShouldReject 表示调用方应当拒绝该请求。
// 注意 OutcomeWouldDeny 返回 false：影子模式下"判定为拒绝"但"不拦截"。
func (r Result) ShouldReject() bool { return r.Outcome == OutcomeDenied }

// ShouldSetHeaders 表示调用方应当下发 X-RateLimit-* 头。
//
// 影子模式一律不下发——**包括判定放行的那些请求**，理由见 Result.DryRun。
// 旁路同理：关闭状态下的响应必须与未挂 limiter 逐字节一致。
func (r Result) ShouldSetHeaders() bool {
	if r.DryRun {
		return false
	}
	return r.Outcome == OutcomeAllowed || r.Outcome == OutcomeDenied
}

// Limiter 是一条可热调、可影子运行的限流通道。
//
// 一个 Limiter 对应一个 class（业务分类，如 business/heartbeat/register），
// 拥有独立的 Redis keyspace 与独立的配置来源。
type Limiter struct {
	b        *bucket
	class    string
	params   ParamsFunc
	obs      Observer
	fallback Params
}

// New 构造一条限流通道。
//
//   - keyPrefix 决定 Redis keyspace，必须以 ':' 结尾，且各 class 之间不得共用，
//     否则不同语义的流量会互相消耗对方的令牌。
//   - fallback 提供**代码内的安全默认值**，仅在配置给出非法值（NaN/±Inf/≤0）时
//     顶上。它自身必须合法，否则读侧防御就没有底。
func New(client *rd.Client, keyPrefix, class string, params ParamsFunc, obs Observer, fallback Params) *Limiter {
	return &Limiter{
		b:        newBucket(client, keyPrefix),
		class:    class,
		params:   params,
		obs:      obs,
		fallback: fallback,
	}
}

// Check 对 key 执行一次限流判定。key 为空时直接旁路——拿不到身份就限不了流，
// 此时**fail-open** 而不是归入某个共享桶：本包服务的都是已鉴权路径，
// 身份缺失属于挂载顺序错误（应当由测试守卫拦住），不应在生产上表现为
// "所有无身份请求共享一个桶"这种更难排查的形态。
func (l *Limiter) Check(key string) Result {
	p := l.params()
	if !p.Enabled || key == "" {
		l.observe(key, OutcomeBypassed)
		return Result{Outcome: OutcomeBypassed}
	}

	rps := SanitizeRPS(p.RPS, l.fallback.RPS)
	burst := SanitizeBurst(p.Burst, l.fallback.Burst)

	br := l.b.allow(key, rps, burst)

	switch {
	case br.degraded:
		// Redis 故障 fail-open。单独一个 outcome 而不是并入 allowed：
		// 「限流层实际上没在工作」必须能在监控上一眼看出来。
		l.observe(key, OutcomeDegraded)
		return Result{Outcome: OutcomeDegraded, Remaining: br.remaining, Burst: burst, DryRun: p.DryRun}
	case br.allowed:
		l.observe(key, OutcomeAllowed)
		return Result{Outcome: OutcomeAllowed, Remaining: br.remaining, Burst: burst, DryRun: p.DryRun}
	case p.DryRun:
		l.observe(key, OutcomeWouldDeny)
		return Result{Outcome: OutcomeWouldDeny, Remaining: br.remaining, RetryAfter: br.retryAfter, Burst: burst, DryRun: true}
	default:
		l.observe(key, OutcomeDenied)
		return Result{Outcome: OutcomeDenied, Remaining: br.remaining, RetryAfter: br.retryAfter, Burst: burst}
	}
}

func (l *Limiter) observe(key string, o Outcome) {
	if l.obs == nil {
		return
	}
	l.obs.Observe(l.class, key, o)
}
