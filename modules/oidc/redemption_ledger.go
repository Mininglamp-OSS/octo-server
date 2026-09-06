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
	// 这条曲线是"客户端是否复用同一张 token 反复兑换"的唯一线上信号 —— 它长期
	// 为零,才谈得上把语义收紧成一次性消费。
	redeemAdmitRepeat redemptionOutcome = "admit_repeat"
	// redeemRejectStaleFirst 首次兑换来得太晚(超过 F)。
	redeemRejectStaleFirst redemptionOutcome = "reject_stale_first"
	// redeemRejectIdle 距上次兑换超过 T,视为已被弃用。
	redeemRejectIdle redemptionOutcome = "reject_idle"
	// redeemDegradedAdmit 台账不可用,按 F 单独判定后放行。
	redeemDegradedAdmit redemptionOutcome = "degraded_admit"
	// redeemDegradedReject 台账不可用,按 F 单独判定后拒绝。
	redeemDegradedReject redemptionOutcome = "degraded_reject"
)

// admitted 该结果是否放行。判定散在调用方就会漂,集中在这里。
func (o redemptionOutcome) admitted() bool {
	return o == redeemAdmitFirst || o == redeemAdmitRepeat || o == redeemDegradedAdmit
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
	}
}

// redemptionPolicy 两个边界。零值不可用,取值经 normalized() 兜底。
type redemptionPolicy struct {
	firstRedeemMaxAge time.Duration // F
	idleWindow        time.Duration // T
}

// normalized 把非正值替换成默认值。
//
// 为什么必须有:这两个值参与的是**准入**判定,F=0 意味着"所有首次兑换都太晚",
// 即全员登录失败。而零值来得很容易 —— 一个漏配的 env、一个测试里手工构造的
// OIDC 结构体。让漏配表现为"用默认策略",而不是"登录全挂"。
func (p redemptionPolicy) normalized() redemptionPolicy {
	if p.firstRedeemMaxAge <= 0 {
		p.firstRedeemMaxAge = defaultFirstRedeemMaxAge
	}
	if p.idleWindow <= 0 {
		p.idleWindow = defaultRedeemIdleWindow
	}
	return p
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

// admitWithoutLedger 台账不可用时的降级判定:只用 F,不看历史。
//
// 方向必须比"只看 exp"紧:Redis 故障不能顺带把重放窗口放大到 15 天。代价是
// 这段时间内重复兑换的客户端若 token 已超过 F,会被拒 —— 用一个独立的
// metric label 暴露出来,而不是混进正常失败里。
func (p redemptionPolicy) admitWithoutLedger(iat, now time.Time) redemptionOutcome {
	if now.Sub(iat) > p.normalized().firstRedeemMaxAge {
		return redeemDegradedReject
	}
	return redeemDegradedAdmit
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
	policy := o.redeemPolicy.normalized()
	if rj == nil {
		// 不可达(调用方在验签成功后才进来),但这里的 fail-open 是发一个会话,
		// 所以按拒绝处理而不是按放行。
		return redeemDegradedReject
	}
	if o.redeemLedger == nil {
		return policy.admitWithoutLedger(rj.IssuedAt, now)
	}
	out, err := o.redeemLedger.Admit(ctx, redemptionDigest(rawToken), rj.IssuedAt, rj.ExpiresAt, now)
	if err != nil {
		o.Warn("OIDC exchange-jwt: redemption ledger unavailable, falling back to the first-redeem ceiling",
			zap.String("trace_id", traceID), zap.Error(err))
		return policy.admitWithoutLedger(rj.IssuedAt, now)
	}
	return out
}
