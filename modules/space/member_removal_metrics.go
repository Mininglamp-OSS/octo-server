package space

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"
)

// 成员移除清理队列的三个 gauge。
//
// 为什么是这条 PR 的一部分而不是留给以后：认领处卡住 attempts 上限 + 扫描把耗尽的
// 工单推到 abandoned，这两件事合起来让失败**终于会终结**——但终结之后依然没有任何
// 人会知道。abandoned 没有自动重驱动，被移除的人会一直留在该 Space 的群里和 IM
// 群订阅里。只做前两件等于把「无限重试的无声」换成「终态的无声」。
//
// 危险的形状不是单条工单卡住，而是一次 IM/DB 退化持续超过重试预算（约 70 分钟），
// 那个窗口里**所有**待处理工单一起变成 abandoned。oldest_pending_age_seconds 是
// 唯一能在它发生**之前**看见的信号：预算耗尽之前，队列积压的年龄会先涨上去。
//
// 与 modules/oidc、modules/sticker 同款：promauto 注册进全局默认 Registry，
// 而 /metrics 端点**已经存在并在服务**（pkg/metrics/http.go，main.go 里启动），
// 所以这三个 gauge 落地即被抓取。
// （早先这里照抄了 oidc 那份「本 PR 不引入 /metrics 端点」的旧注释而没有核实——
// 那句话在 oidc 写下时是真的，现在不是了。）
// 刻意不加 space_id / uid 这类高基维 label —— 会撑爆 Prometheus 内存。
const removalMetricNamespace = "space"

var (
	removalCleanupPendingGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: removalMetricNamespace,
		Name:      "member_removal_cleanup_pending",
		Help:      "Number of member-removal cleanup jobs still pending.",
	})
	removalCleanupOldestPendingGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: removalMetricNamespace,
		Name:      "member_removal_cleanup_oldest_pending_age_seconds",
		Help: "Age in seconds of the oldest pending member-removal cleanup job. " +
			"Rises before jobs exhaust their retry budget, so it is the leading indicator.",
	})
	removalCleanupAbandonedGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: removalMetricNamespace,
		Name:      "member_removal_cleanup_abandoned",
		Help: "Number of member-removal cleanup jobs abandoned after exhausting retries. " +
			"Nothing re-drives these; a non-zero value means removed members are still in their groups.",
	})
)

// removalCleanupStats 是一次队列快照。
type removalCleanupStats struct {
	Pending             int64 `db:"pending"`
	Abandoned           int64 `db:"abandoned"`
	OldestPendingAgeSec int64 `db:"oldest_pending_age_sec"`
}

// queryMemberRemovalCleanupStats 一次查询取回三个指标。
//
// 这是一次全表聚合：MIN(CASE WHEN status=0 THEN created_at END) 没有任何索引能覆盖。
// 稳态下表不大（14 天保留期 + 每小时 purge），但**恰恰在这些指标最有价值的时刻**
// ——一次大规模放弃之后——表会永久变大，聚合也就永久变慢。所以刻意跑得比扫描稀疏，
// 见 removalMetricsInterval。
func (d *DB) queryMemberRemovalCleanupStats() (*removalCleanupStats, error) {
	var stats removalCleanupStats
	_, err := d.session.SelectBySql(
		"SELECT "+
			"IFNULL(SUM(status=?),0) AS pending, "+
			"IFNULL(SUM(status=?),0) AS abandoned, "+
			// 必须用 NOW(3) 而不是 UTC_TIMESTAMP(3)：created_at 从不由 Go 写入，
			// 走的是列默认值 CURRENT_TIMESTAMP(3)，即 MySQL 会话时区。拿 UTC 去减
			// 一个会话时区的时间戳，在 TZ=Asia/Shanghai + DSN loc=Local 的部署下会
			// 得到 -28799 秒，而且要等积压满 8 小时才转正——这个 gauge 恰恰是用来
			// 在大规模放弃**发生之前**报警的，在那个点上它是坏的。
			// 两边同为会话时区即自洽。
			"IFNULL(TIMESTAMPDIFF(SECOND, MIN(CASE WHEN status=? THEN created_at END), NOW(3)),0) "+
			"AS oldest_pending_age_sec "+
			"FROM space_member_removal_cleanup",
		removalCleanupPending, removalCleanupAbandoned, removalCleanupPending,
	).Load(&stats)
	if err != nil {
		return nil, fmt.Errorf("space: query removal cleanup stats: %w", err)
	}
	return &stats, nil
}

// refreshMemberRemovalCleanupMetrics 把队列快照推进 gauge。
func (s *Space) refreshMemberRemovalCleanupMetrics() {
	stats, err := s.db.queryMemberRemovalCleanupStats()
	if err != nil {
		s.Warn("采集成员移除清理队列指标失败", zap.Error(err))
		return
	}
	removalCleanupPendingGauge.Set(float64(stats.Pending))
	removalCleanupAbandonedGauge.Set(float64(stats.Abandoned))
	removalCleanupOldestPendingGauge.Set(float64(stats.OldestPendingAgeSec))
}
