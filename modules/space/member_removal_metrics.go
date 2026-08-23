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
// 本 PR 不引入 /metrics 端点（那是独立的基础设施改动）。
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
// 全表聚合，但这张表有保留期清理兜底，稳态下很小；而且每分钟只跑一次，
// 与耗尽扫描同一轮，不额外加一个定时器。
func (d *DB) queryMemberRemovalCleanupStats() (*removalCleanupStats, error) {
	var stats removalCleanupStats
	_, err := d.session.SelectBySql(
		"SELECT "+
			"IFNULL(SUM(status=?),0) AS pending, "+
			"IFNULL(SUM(status=?),0) AS abandoned, "+
			"IFNULL(TIMESTAMPDIFF(SECOND, MIN(CASE WHEN status=? THEN created_at END), UTC_TIMESTAMP(3)),0) "+
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
