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
// KEYS[1]: ZSet 键
// ARGV[1]: member（业务标识，如 robotID）
// ARGV[2]: topN（保留的最大成员数）
// ARGV[3]: ttl（秒）
const offendersScript = `
local key = KEYS[1]
local member = ARGV[1]
local topN = tonumber(ARGV[2])
local ttl = tonumber(ARGV[3])

redis.call("ZINCRBY", key, 1, member)
-- 按 score 升序，删掉除最高 topN 之外的全部；集合规模 <= topN 是结构性保证，
-- 不依赖调用方记得清理。
redis.call("ZREMRANGEBYRANK", key, 0, -topN - 1)
redis.call("EXPIRE", key, ttl)
return 1
`

const (
	// defaultOffendersTopN 是保留的 offender 数量上限。50 足够回答「是哪几个 bot」
	// ——生产事故的形态是单个 bot 打爆配额，而不是几百个 bot 各超一点。
	defaultOffendersTopN = 50
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
	_ = r.script.Run(r.client, []string{r.prefix + class}, key, r.topN, int(r.ttl.Seconds())).Err()
}

// Top 返回该 class 当前的 offender 排行（score 降序），供管理端/排查使用。
func (r *OffenderRecorder) Top(class string, n int) ([]rd.Z, error) {
	if r == nil || r.client == nil {
		return nil, nil
	}
	if n <= 0 || n > r.topN {
		n = r.topN
	}
	return r.client.ZRevRangeWithScores(r.prefix+class, 0, int64(n-1)).Result()
}
