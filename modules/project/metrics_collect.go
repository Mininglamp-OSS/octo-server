package project

import (
	"time"

	"go.uber.org/zap"
)

// Metrics collection lives in its own file, separate from reconcile.go, and that separation is
// deliberate rather than cosmetic.
//
// reconcile.go's guard test (TestReconcileQueriesAreBounded) requires EVERY query in that file
// to carry `LIMIT ?`, with no exemptions. An earlier version of the guard exempted anything
// containing COUNT(*) so that the two whole-table gauges below could live alongside the scans —
// and that exemption is exactly how an unbounded `SELECT COUNT(*) ... WHERE EXISTS (...)` over
// the entire cleanup table got through review. A guard whose exemption covers the shape you
// most need to catch reads as coverage while providing none.
//
// So the gauges moved here instead, and they carry their own guard
// (TestMetricsCollectionAggregatesAreRegistered) which whitelists them BY STATEMENT. Adding a
// query here means editing that list — an explicit, reviewable act — rather than matching a
// pattern that silently admits new offenders.
//
// Why these two genuinely cannot be paged: they report totals. A total is a whole-table
// aggregate by definition; paging it would just be summing the pages. What bounds their cost is
// the interval, not a LIMIT — they run on MetricsInterval (15 min by default), sparser than the
// reconcile scans, the same trade-off modules/space documents for its removal-queue aggregate
// (removalMetricsInterval).

// refreshDistributionMetrics samples the project and member-count distribution.
//
// On a sparser tick than the scans: these are whole-table aggregates, and they get
// slowest exactly when the numbers matter most (after a backlog). modules/space put
// its removal-queue aggregate on a slower schedule than its worker for the same
// reason.
//
// Be clear about what the LIMIT on the distribution query does and does not do, because
// the scans above use LIMIT to bound COST and this one does not: LIMIT applies AFTER
// GROUP BY, so MySQL still aggregates the whole table and the LIMIT only caps how many
// rows come back — i.e. how many histogram samples are taken, not how much is read. The
// interval is what bounds the cost here.
//
// The sample is also not random: with no ORDER BY the rows come back in whatever order
// the aggregate produced them, so a deployment with more projects than ReconcileLimit
// keeps sampling roughly the same subset. Fine for a distribution read as a trend, wrong
// if anyone ever reads it as a population statistic.
func (p *Project) refreshDistributionMetrics() {
	if !metricsRunning.CompareAndSwap(false, true) {
		return
	}
	defer metricsRunning.Store(false)
	defer func() {
		if r := recover(); r != nil {
			p.Error("项目指标采集 panic", zap.Any("recover", r))
		}
	}()
	start := time.Now()
	defer func() { reconcileDuration.WithLabelValues("metrics").Observe(time.Since(start).Seconds()) }()

	var projects int64
	if err := p.db.session.SelectBySql(
		"SELECT COUNT(*) FROM `octo_project` WHERE status = ?", StatusNormal,
	).LoadOne(&projects); err != nil {
		p.Warn("采集项目总数失败", zap.Error(err))
		return
	}
	projectTotal.Set(float64(projects))

	var members int64
	if err := p.db.session.SelectBySql(
		"SELECT COUNT(*) FROM `octo_project_member` WHERE status = ?", MemberStatusActive,
	).LoadOne(&members); err != nil {
		p.Warn("采集项目成员总数失败", zap.Error(err))
		return
	}
	memberTotal.Set(float64(members))

	var rows []*distributionRow
	if _, err := p.db.session.SelectBySql(
		"SELECT COUNT(*) AS member_count FROM `octo_project_member` "+
			"WHERE status = ? GROUP BY project_id LIMIT ?",
		MemberStatusActive, p.cfg.ReconcileLimit,
	).Load(&rows); err != nil {
		p.Warn("采集项目成员分布失败", zap.Error(err))
		return
	}
	for _, row := range rows {
		memberCountDistribution.Observe(float64(row.MemberCount))
	}
}
