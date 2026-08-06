package ratelimit

import (
	"time"

	rd "github.com/go-redis/redis"
)

// offendersScript 累加一个 offender 的计数并**结构性地**把集合裁剪到 top N。
//
// 为什么要 Lua：ZINCRBY + ZREMRANGEBYRANK + EXPIRE 三条命令若分开发，拒绝高峰期
// （生产实测曾出现 19 秒内 966 次拒绝）就是三倍往返；合成一次往返后，观测开销与
// 拒绝次数同阶但常数最小。
//
// 为什么要裁剪：这是**唯一**持有业务身份（robotID）的观测结构，若不裁剪，
// 它就变成了一个无界的身份集合——那正是我们拒绝把 robotID 放进 Prometheus label
// 的理由，不能在 Redis 里再犯一次。
//
// **但每次写入都裁到 topN 会永久饿死新来的 offender。** 第一版就是那样写的:
// 集合已有 topN 个成员且分数都 >= 2 时,新成员以 1 分进来、成为唯一最低分,
// 同一趟 Lua 里的 ZREMRANGEBYRANK 立刻把它删掉;下次再犯还是从 1 分开始、再被删掉。
// 于是**一个在 50 个之后才开始作妖的 bot 会永远不出现在名单里**——而整个灰度计划
// 恰恰依赖影子模式回答"这个配额会拒谁"。（三位 reviewer 里两位独立指出了这条。）
//
// 修法:高水位裁剪。允许集合长到 2*topN 才裁回 topN,新成员因此有一整个水位区间
// 用来积累分数、公平竞争。代价是常态占用最多翻倍(100 个成员)——仍然**结构性有界**,
// 而这正是裁剪存在的唯一理由。
//
// KEYS[1]: ZSet 键
// ARGV[1]: member（业务标识，如 robotID）
// ARGV[2]: topN（裁剪后保留的成员数）
// ARGV[3]: ttl（秒）
// ARGV[4]: highWater（超过该规模才触发裁剪）
const offendersScript = `
local key = KEYS[1]
local member = ARGV[1]
local topN = tonumber(ARGV[2])
local ttl = tonumber(ARGV[3])
local highWater = tonumber(ARGV[4])

redis.call("ZINCRBY", key, 1, member)
-- 只在超过高水位时裁剪,且裁到 topN。不是每次写入都裁——那会让新成员在积累到
-- 有意义的分数之前就被删掉(见上方注释)。
if redis.call("ZCARD", key) > highWater then
    redis.call("ZREMRANGEBYRANK", key, 0, -topN - 1)
end
redis.call("EXPIRE", key, ttl)
return 1
`

const (
	// defaultOffendersTopN 是裁剪后保留的 offender 数量。50 足够回答「是哪几个 bot」
	// ——生产事故的形态是单个 bot 打爆配额，而不是几百个 bot 各超一点。
	defaultOffendersTopN = 50
	// defaultOffendersHighWaterFactor 决定裁剪触发的规模(topN 的倍数)。
	// 取 2:新 offender 有 topN 个名额的缓冲期用来积累分数,不会一进来就被裁掉;
	// 同时上界仍然是常数倍,没有失去有界性。
	defaultOffendersHighWaterFactor = 2
	// defaultOffendersTTL 让统计窗口自然滚动：不再产生拒绝的 class，其名单会过期
	// 消失，避免运维看到一个早已恢复的 bot 仍挂在榜首。
	defaultOffendersTTL = 24 * time.Hour
)

// OffenderRecorder 把「谁被拒了」记进一个**有界**的 Redis ZSet，用于事故时定位到
// 具体 bot。它与 Prometheus 指标是互补的两层：
//
//	Prometheus  —— 无身份、低基数，回答「整体拒了多少、趋势如何」
//	OffenderZSet —— 有身份、有界，回答「具体是哪几个」
//
// 之所以不把身份塞进 Prometheus：生产实测活跃 bot 2903 个（Redis bot:heartbeat:*，
// TTL 60s）且随业务增长无上界，作为 label 会产生数万条随时间 churn 的 series。
type OffenderRecorder struct {
	client *rd.Client
	script *rd.Script
	prefix string
	topN   int
	ttl    time.Duration
}

// NewOffenderRecorder 构造记录器。prefix 必须以 ':' 结尾。
func NewOffenderRecorder(client *rd.Client, prefix string) *OffenderRecorder {
	return &OffenderRecorder{
		client: client,
		script: rd.NewScript(offendersScript),
		prefix: prefix,
		topN:   defaultOffendersTopN,
		ttl:    defaultOffendersTTL,
	}
}

// Record 只应在**拒绝或影子拒绝**时调用，绝不能每请求调用——那会让观测本身
// 成为一条与业务流量同阶的 Redis 写路径。
//
// 错误被吞掉：观测失败不应影响业务判定。degraded 的信号由 Prometheus 那一层
// （OutcomeDegraded）承担，此处再记一次只会在故障期放大日志。
func (r *OffenderRecorder) Record(class, key string) {
	if r == nil || r.client == nil || key == "" {
		return
	}
	highWater := r.topN * defaultOffendersHighWaterFactor
	_ = r.script.Run(r.client, []string{r.prefix + class},
		key, r.topN, int(r.ttl.Seconds()), highWater).Err()
}

// Top 返回该 class 当前的 offender 排行（score 降序），供排查使用。
//
// **目前没有生产调用方**，这是刻意的:brief 明确决定不为它建管理端点（多一个鉴权面、
// 而事故期直接 `redis-cli ZREVRANGE ratelimit:bot:offenders:<class> 0 -1 WITHSCORES`
// 就够）。保留这个方法的理由是它把 key 拼接与降序语义收在包内一处——将来若要接
// 管理端点或诊断命令，是唯一该走的读路径，不必在调用方重新拼 key。
//
// 上界用 highWater 而非 topN:高水位裁剪允许集合暂时长到 2*topN,若这里仍夹到 topN,
// 排查时就会看不到水位区间内那些**最新出现**的 offender——而那恰恰是最需要看的。
func (r *OffenderRecorder) Top(class string, n int) ([]rd.Z, error) {
	if r == nil || r.client == nil {
		return nil, nil
	}
	max := r.topN * defaultOffendersHighWaterFactor
	if n <= 0 || n > max {
		n = max
	}
	return r.client.ZRevRangeWithScores(r.prefix+class, 0, int64(n-1)).Result()
}
