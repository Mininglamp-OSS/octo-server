package oidc

// redemption_ledger.go — /exchange-jwt 的兑换台账。
//
// 把新鲜度的锚点从"上游签发时刻"换成"兑换行为本身"。
//
// 原先的判定是 `now - iat <= 10min`(已移除的 bearerJWTMaxAge)。锚点选错了:
// iat 是上游签发的时刻,跟"用户什么时候真的来兑换"没有关系,于是两头都不对 ——
// 登录后隔 36 分钟才兑换的合法客户端被拒(返回的 401 与"凭据无效"不可区分),
// 而在签发后 10 分钟内抓到 token 的攻击者照样能兑。
//
// 台账为每张兑换过的 token 记一条记录,用两个边界代替原来那一个:
//
//	F(firstRedeemMaxAge)—— 首次兑换距 iat 的上限。防 token 在**首次使用前**被窃取:
//	                       没有这道,一张从未兑换过的 token 在 exp(约 15 天)内
//	                       任何时刻都能开一个新账号,正是当初加那道 10 分钟上限时堵的洞。
//	T(idleWindow)     —— 相邻两次兑换的间隔上限。防 token 在用户**弃用/登出后**被
//	                       窃取:上游的吊销状态我方查不到,但"这张 token 已经没人在
//	                       用了"是我方自己能观察到的。
//
// 合法客户端只要在持续使用,每次兑换都刷新 T,永远不会被拒;它是否复用同一张
// token 兑换多次,本设计不需要预先知道。
//
// **记录丢失(Redis 重启无持久化 / maxmemory 淘汰)与"从未兑换过"不可区分。**
// 后果是:一张仍在使用、但 iat 已超过 F 的 token 会以 reject_stale_first 被拒,
// 客户端必须重走一次 SSO。这是**有意选择的方向** —— 台账就是这条路径的安全状态,
// 状态丢失后要求重新认证是正确的一侧;反过来"记录没了就放行"等于让一次 Redis
// 故障把 F 关掉。运维侧的信号是 reject_stale_first 突然成批出现(正常时它稀疏),
// 该告警指向的是 Redis 数据丢失,不是客户端行为变化。
//
// **记录的 TTL 必须是 token 自己的剩余寿命,不是 T。** 若 TTL=T,记录过期后这张
// token 在下次兑换时会被当成"首次兑换"重新走 F,而 F 是从 iat 起算的 —— 老 token
// 会被 F 拒掉,看似也对;但只要 T > F,记录就会在 F 还没到期时先消失,重复兑换的
// 客户端反而被当成首次而拒。语义必须由记录里的 last_at 表达,不能由 key 的存活表达。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
	"go.uber.org/zap"
)

// redemptionKeyPrefix 兑换台账的 Redis key 前缀。key 的可变部分是 token 的
// sha256 十六进制摘要 —— 不存 token 本身:Redis 里一条明文 assertion 等于一份
// 可直接使用的凭据,而摘要不可逆,只够做"是不是同一张"的比对。
const redemptionKeyPrefix = "oidc:bjwt:redeem:"

const (
	// defaultFirstRedeemMaxAge F 的默认值。
	//
	// 24 小时覆盖"登录当天才打开客户端"这种正常间隔(线上观察到的失败是 36 分钟),
	// 同时把"首次使用前被窃取"的窗口从 exp 的约 15 天压到 1 天。
	defaultFirstRedeemMaxAge = 24 * time.Hour

	// defaultRedeemIdleWindow T 的默认值。
	//
	// 7 天:比"周末不开机"长,比 exp 短。它只在 token 被弃用后起作用,持续使用
	// 的客户端碰不到它。
	defaultRedeemIdleWindow = 7 * 24 * time.Hour

	// redemptionRecordMaxTTL 记录 TTL 的上限,防一张 exp 离谱的 token(签发方
	// 写错单位、或密钥泄漏后被签成百年有效)在 Redis 里占一条近乎永久的记录。
	//
	// 截断不会放松判定:被截掉记录的 token,其 iat 至少也在 maxTTL 之前,下次
	// 兑换会以"首次"进入 F 的判定而被 F 拒。
	redemptionRecordMaxTTL = 30 * 24 * time.Hour
)

// redemptionOutcome 一次兑换判定的结果。同时是 metric 的 label 值,所以取值必须
// 与 redemptionOutcomeLabels() 一致。
type redemptionOutcome string

const (
	// redeemAdmitFirst 首次兑换,在 F 之内,已落账。
	redeemAdmitFirst redemptionOutcome = "admit_first"
	// redeemAdmitRepeat 重复兑换,距上次在 T 之内,已刷新 last_at。
	//
	// 这条曲线回答的是**是否存在重复兑换**,不区分"客户端自己复用"与"有人在重放"
	// —— 台账只存 last_at,两者产生的增量完全相同。所以它只能作否定判据:长期为
	// 零,才谈得上把语义收紧成一次性消费;非零时它不构成"客户端在正常复用"的证据,
	// 要判断是谁在兑换,得先有能区分调用方的记录(那会是另一个取舍:存 IP/uid
	// 等于给凭据加一份用户行为记录)。
	redeemAdmitRepeat redemptionOutcome = "admit_repeat"
	// redeemRejectStaleFirst 首次兑换来得太晚(超过 F)。
	redeemRejectStaleFirst redemptionOutcome = "reject_stale_first"
	// redeemRejectIdle 距上次兑换超过 T,视为已被弃用。
	redeemRejectIdle redemptionOutcome = "reject_idle"
	// redeemDegradedAdmit 台账**报错**(Redis 不可用),降级判定后放行。
	redeemDegradedAdmit redemptionOutcome = "degraded_admit"
	// redeemDegradedReject 台账**报错**,降级判定后拒绝。
	redeemDegradedReject redemptionOutcome = "degraded_reject"

	// redeemUnconfiguredAdmit / redeemUnconfiguredReject 台账**根本没装上**,
	// 降级判定后的结果。
	//
	// 与 degraded_* 分开,是因为两者对运维意味着完全不同的事:degraded 是"Redis
	// 现在不好",会自己恢复;unconfigured 是"这个部署压根没有台账",T 永远不生效,
	// 且不会自愈。共用一组 label 的话,一次接线回归看起来就只是 Redis 在抖。
	redeemUnconfiguredAdmit  redemptionOutcome = "unconfigured_admit"
	redeemUnconfiguredReject redemptionOutcome = "unconfigured_reject"
)

// admitted 该结果是否放行。判定散在调用方就会漂,集中在这里。
func (o redemptionOutcome) admitted() bool {
	return o == redeemAdmitFirst || o == redeemAdmitRepeat ||
		o == redeemDegradedAdmit || o == redeemUnconfiguredAdmit
}

// redemptionOutcomeLabels metric 预热用的全部取值。
func redemptionOutcomeLabels() []string {
	return []string{
		string(redeemAdmitFirst),
		string(redeemAdmitRepeat),
		string(redeemRejectStaleFirst),
		string(redeemRejectIdle),
		string(redeemDegradedAdmit),
		string(redeemDegradedReject),
		string(redeemUnconfiguredAdmit),
		string(redeemUnconfiguredReject),
	}
}

// redemptionPolicy 两个边界。零值不可用,取值经 normalized() 兜底。
type redemptionPolicy struct {
	firstRedeemMaxAge time.Duration // F
	idleWindow        time.Duration // T
}

// normalized 把两个边界收敛成**可执行的取值**:非正回落默认、截到整秒且不低于
// 1 秒、T 不超过记录能存活的上限。
//
// 为什么必须有:这两个值参与的是**准入**判定,F=0 意味着"所有首次兑换都太晚",
// 即全员登录失败。而零值来得很容易 —— 一个漏配的 env、一个测试里手工构造的
// OIDC 结构体。让漏配表现为"用默认策略",而不是"登录全挂"。
//
// 整秒截断与 1 秒下限是同一类保护,针对的是**亚秒取值**:判定在 Lua 里以整秒
// 比较,`int64(500ms/time.Second)` 是 0,于是脚本比的是 `now-iat > 0` —— 任何
// 一秒前签发的 token 都被拒,就是 F=0 那个全员登录失败,只是绕了一圈。截断放在
// 这里而不是转换处,是为了让**降级路径**(Go 里按 Duration 比较)与 Lua 用的是
// 同一个值,两条路径不会对同一张 token 得出不同结论。
//
// **两个边界都以 redemptionRecordMaxTTL 为上限**,理由同源但方向不同:
//
//   - T 长过它就不可执行:记录先没了,超时的兑换会以"首次兑换超过 F"的名义被拒
//     —— 拒得对,但归因是错的,运维会去调 F。
//   - F 长过它会**反过来废掉 T**:记录在第 30 天被 TTL 截掉,第 31 天的兑换找不到
//     记录、被当成首次,而 `31d <= F` 于是放行 —— 一个 7 天的空闲窗口就这样被
//     一个 60 天的 F 绕过去了。F 不该比"我们还记得这张 token"更长。
//
// 收敛到能执行的值,启动日志打印的也就是真正生效的值。
func (p redemptionPolicy) normalized() redemptionPolicy {
	p.firstRedeemMaxAge = normalizeBound(p.firstRedeemMaxAge, defaultFirstRedeemMaxAge, redemptionRecordMaxTTL)
	p.idleWindow = normalizeBound(p.idleWindow, defaultRedeemIdleWindow, redemptionRecordMaxTTL)
	return p
}

// normalizeBound 单个边界的收敛规则。max<=0 表示不设上限。
func normalizeBound(v, def, max time.Duration) time.Duration {
	if v <= 0 {
		v = def
	}
	v = v.Truncate(time.Second)
	if v < time.Second {
		v = time.Second
	}
	if max > 0 && v > max {
		v = max
	}
	return v
}

// loadRedemptionPolicy 从环境读取两个边界。
//
// 解析失败或非正值一律回落默认值(getDurationWithAlias 已处理解析失败,
// normalized 处理非正值)—— 见 normalized 的说明:这里的"严格"会变成登录故障。
func loadRedemptionPolicy() redemptionPolicy {
	return redemptionPolicy{
		firstRedeemMaxAge: getDurationWithAlias("OCTO_OIDC_BEARER_JWT_FIRST_REDEEM_MAX_AGE", "", defaultFirstRedeemMaxAge),
		idleWindow:        getDurationWithAlias("OCTO_OIDC_BEARER_JWT_REDEEM_IDLE_WINDOW", "", defaultRedeemIdleWindow),
	}.normalized()
}

// fallbackAdmits 拿不到台账时的降级判定:不看历史,只按 iat 判一个上限。
//
// 上限取 **min(F, T)**,而不是单独的 F。降级路径的全部正当性在于"它绝不比正常
// 路径松",而正常路径对一张有记录的 token 用的是 T:若运维把 T 配得比 F 短
// (合法配置:首次可以来得晚,复用必须频繁),只用 F 就会出现"Redis 挂了反而更
// 好过"——两小时前用过的 token 正常时是 reject_idle,降级时却放行。取两者较小值
// 让这个方向不可能发生;默认值下 F=24h < T=7d,min 就是 F,行为不变。
//
// 方向也必须比"只看 exp"紧:Redis 故障不能顺带把重放窗口放大到 15 天。代价是
// 这段时间内 token 已超过该上限的客户端会被拒 —— 用独立的 metric label 暴露,
// 不混进正常失败里。
func (p redemptionPolicy) fallbackAdmits(iat, now time.Time) bool {
	// 这里仍收敛一次:调用方可能是一个零值 policy(直接构造的 OIDC),而这条路径
	// 的 fail-open 后果是发会话。normalized 是幂等的,重复调用不改变结果。
	n := p.normalized()
	bound := n.firstRedeemMaxAge
	if n.idleWindow < bound {
		bound = n.idleWindow
	}
	return now.Sub(iat) <= bound
}

// redemptionLedger 兑换台账。
//
// 只由 /exchange-jwt 的 handler 调用。**不能**放进 BearerJWTVerifier:
// api_exchange.go 用同一个验签方法做凭据归属**分类**("这是不是一张发错端点的
// 业务 JWT"),那条路径不是兑换,却会因此在台账里留下或刷新一条记录 —— 一张投错
// 端点的 token 会因此获得一次它本不该有的续命。验签保持无副作用,副作用只在
// 兑换这一处。
type redemptionLedger interface {
	// Admit 判定一次兑换,放行时落账。
	//
	// 判定与落账必须在同一次往返里完成(实现用 Lua),否则并发兑换会各自读到
	// 旧的 last_at 再各自写回,窗口判定失去意义。
	Admit(ctx context.Context, digest string, iat, exp, now time.Time) (redemptionOutcome, error)
}

// redemptionDigest token 的 sha256 十六进制摘要,作为台账 key 的可变部分。
func redemptionDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func redemptionKey(digest string) string { return redemptionKeyPrefix + digest }

// luaRedemptionAdmit 原子判定 + 落账。
//
//	KEYS[1] = 台账 key
//	ARGV[1] = now(unix 秒)
//	ARGV[2] = iat(unix 秒)
//	ARGV[3] = F(秒)
//	ARGV[4] = T(秒)
//	ARGV[5] = 记录 TTL(秒,= min(exp-now, redemptionRecordMaxTTL))
//
// 值只存 last_at。不存计数/IP:兑换次数由 admit_repeat 这条曲线表达,而按 IP 拦
// 会误伤移动网络换 IP 的正常客户端 —— 存了却不据以判定的字段,只是一份多余的
// 用户行为记录。
//
// 记录损坏(tonumber 得 nil)按"不存在"处理:它与 key 被删等价,而能删 key 的人
// 也能写坏它,所以这不是一条新的降级路径。
var luaRedemptionAdmit = rd.NewScript(`
local now = tonumber(ARGV[1])
local last = tonumber(redis.call("GET", KEYS[1]))
if last == nil then
  if now - tonumber(ARGV[2]) > tonumber(ARGV[3]) then
    return "reject_stale_first"
  end
  redis.call("SET", KEYS[1], ARGV[1], "EX", tonumber(ARGV[5]))
  return "admit_first"
end
if now - last > tonumber(ARGV[4]) then
  return "reject_idle"
end
redis.call("SET", KEYS[1], ARGV[1], "EX", tonumber(ARGV[5]))
return "admit_repeat"
`)

// redisRedemptionLedger 生产实现。
//
// 与 redisStateStore / redisIDTokenStore 同构:持独立 *redis.Client(go-redis v6),
// Read/WriteTimeout 提供命令级超时,网络分区时不把登录路径挂死 —— 超时会走
// handler 的降级分支,而不是让请求悬着。
type redisRedemptionLedger struct {
	client *rd.Client
	policy redemptionPolicy
}

func newRedisRedemptionLedger(ctx *config.Context, policy redemptionPolicy) *redisRedemptionLedger {
	client := octoredis.NewInstrumentedClient(ctx.GetConfig(), func(o *rd.Options) {
		o.MaxRetries = 3
		o.ReadTimeout = 3 * time.Second
		o.WriteTimeout = 3 * time.Second
		o.DialTimeout = 3 * time.Second
	})
	return &redisRedemptionLedger{client: client, policy: policy.normalized()}
}

// Admit 见 redemptionLedger.Admit。
//
// 接受 context.Context 满足接口契约,但 go-redis v6 的命令 API 不支持 context
// 取消;cancellation 由 Read/WriteTimeout 替代(与 redisStateStore 一致)。
func (l *redisRedemptionLedger) Admit(_ context.Context, digest string, iat, exp, now time.Time) (redemptionOutcome, error) {
	if l == nil || l.client == nil {
		return "", errors.New("oidc: redemption ledger is not configured")
	}
	if digest == "" {
		return "", errors.New("oidc: redemption digest is empty")
	}
	ttl := recordTTL(exp, now)
	// 自己再收敛一次,不依赖构造方。**这次改动修的那个亚秒 bug 就是这种形态**:
	// 一个"看起来合法"的值绕过收敛直接进了脚本,而这里没收敛的后果是 ARGV 里出现
	// 0,脚本比的是 `now-iat > 0`,每一次登录都被拒。normalized 幂等,代价为零。
	p := l.policy.normalized()
	res, err := luaRedemptionAdmit.Run(l.client, []string{redemptionKey(digest)},
		strconv.FormatInt(now.Unix(), 10),
		strconv.FormatInt(iat.Unix(), 10),
		strconv.FormatInt(int64(p.firstRedeemMaxAge/time.Second), 10),
		strconv.FormatInt(int64(p.idleWindow/time.Second), 10),
		strconv.FormatInt(int64(ttl/time.Second), 10),
	).Result()
	if err != nil {
		return "", fmt.Errorf("oidc: redis redemption admit: %w", err)
	}
	raw, ok := res.(string)
	if !ok {
		return "", fmt.Errorf("oidc: redemption admit returned %T, want string", res)
	}
	out := redemptionOutcome(raw)
	switch out {
	case redeemAdmitFirst, redeemAdmitRepeat, redeemRejectStaleFirst, redeemRejectIdle:
		return out, nil
	default:
		// 脚本只可能返回上面四种。返回别的说明脚本被改坏了,当成台账不可用走降级,
		// 而不是把一个无法解释的值当作放行。
		return "", fmt.Errorf("oidc: redemption admit returned unknown outcome %q", raw)
	}
}

// Close 释放底层 Redis 连接池,在模块/进程优雅关闭时调用。
func (l *redisRedemptionLedger) Close() error {
	if l == nil || l.client == nil {
		return nil
	}
	if err := l.client.Close(); err != nil {
		return fmt.Errorf("oidc: redis redemption ledger close: %w", err)
	}
	return nil
}

// recordTTL 记录的存活时间 = token 的剩余寿命,上限 redemptionRecordMaxTTL。
//
// 至少 1 秒:exp 已过期的 token 在验签阶段就被拒了,走不到这里,但 TTL<=0 会被
// Redis 当作参数错误,不如就地钳住。
func recordTTL(exp, now time.Time) time.Duration {
	ttl := exp.Sub(now)
	if ttl > redemptionRecordMaxTTL {
		ttl = redemptionRecordMaxTTL
	}
	if ttl < time.Second {
		ttl = time.Second
	}
	return ttl
}

// admitRedemption 兑换准入判定,含台账不可用时的降级。
//
// 未配置台账(o.redeemLedger == nil)与台账报错走同一条降级路径:两者对这次请求
// 的意义相同 —— "拿不到历史",而准入不能因为拿不到历史就无条件放行。
func (o *OIDC) admitRedemption(ctx context.Context, rawToken string, rj *RedeemedBearerJWT, now time.Time, traceID string) redemptionOutcome {
	if rj == nil {
		// 不可达(调用方在验签成功后才进来),但这里的 fail-open 是发一个会话,
		// 所以按拒绝处理而不是按放行。
		return redeemDegradedReject
	}
	if o.redeemLedger == nil {
		// 不是"Redis 现在不好",是这个部署没有台账 —— T 永远不生效,且不会自愈。
		// 生产上不可达(New() 在端点会挂载时必装),所以这条日志的读者是接线回归,
		// 该吵就吵;metric 也用独立的 unconfigured_* 与 Redis 故障区分开。
		o.Warn("OIDC exchange-jwt: no redemption ledger configured; admission falls back to "+
			"the iat ceiling and the idle window is not enforced at all",
			zap.String("trace_id", traceID))
		if o.redeemPolicy.fallbackAdmits(rj.IssuedAt, now) {
			return redeemUnconfiguredAdmit
		}
		return redeemUnconfiguredReject
	}
	out, err := o.redeemLedger.Admit(ctx, redemptionDigest(rawToken), rj.IssuedAt, rj.ExpiresAt, now)
	if err != nil {
		o.Warn("OIDC exchange-jwt: redemption ledger unavailable, falling back to the iat ceiling",
			zap.String("trace_id", traceID), zap.Error(err))
		if o.redeemPolicy.fallbackAdmits(rj.IssuedAt, now) {
			return redeemDegradedAdmit
		}
		return redeemDegradedReject
	}
	return out
}
