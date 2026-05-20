package oidc

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// 本文件登记 OIDC 模块的全部 Prometheus 指标(7 项)。
//
// 设计取舍:
//   - 注册到全局默认 Registry(promauto.New*),由后续基础设施 PR 暴露
//     `/metrics` 端点。本 PR 不引入 HTTP 端点,避免越界。
//   - Counter 用 *Vec 带 result 维度,便于 Grafana 切分成功/失败比例。
//   - Histogram 仅 callback 一项,buckets 覆盖 50ms ~ 5s,匹配 Aegis Discovery +
//     token exchange + 可能的 /userinfo 拉取的真实 P99。
//   - **不**给 metric 加 issuer/uid 这类高基维 label —— 单 issuer(Aegis)够用,
//     uid 会爆炸 Prometheus 内存。

const metricNamespace = "oidc"

// callbackResultLabels callback handler 的全部出口标签,审计图表枚举用。
//
// 任何分支引入新失败原因都应回到这里加常量,Grafana dashboard 才能稳定。
//
// init 时会用每个 label 调一次 Add(0),保证未触发的 result 也作为零值序列出现,
// dashboard 上"零次"和"不存在"得以区分。
func callbackResultLabels() []string {
	return []string{
		"ok",
		"state_invalid",
		"idp_error",
		"missing_code",
		"exchange_fail",
		"verify_fail",
		"nonce_mismatch",
		"resolve_fail",
		"issue_fail",
		"identity_insert_fail",
		"race_recovered",
		"set_authcode_fail",
		"rate_limited",
		"bind_pending", // PR4:autolink 失败 + Bind 接管成功,跳 bind 页
		"other_fail",
	}
}

func stateConsumeResultLabels() []string  { return []string{"ok", "miss"} }
func logoutResultLabels() []string        { return []string{"ok", "kick_fail", "revoke_fail"} }
func syncTickResultLabels() []string      { return []string{"ran", "lock_held", "lock_err"} }
func syncProcessedResultLabels() []string { return []string{"ok", "invalid_grant", "transient", "panic"} }
func syncVerificationSyncedResultLabels() []string {
	// YUJ-405:SyncWorker rotate 后 /userinfo → upsert 的结果维度。
	// YUJ-409 Round 2:新增 sub_mismatch(ownership 校验失败)和 fetch_nil
	// (/userinfo 返 (nil,nil) 防御)两个分支。
	return []string{"upserted", "skipped_unverified", "fetch_failed", "upsert_failed", "sub_mismatch", "fetch_nil"}
}

// bindEndpointLabels 自助绑定流程 5 个端点的标签维度。
// callback "bind_pending"(callback 接管入口)单独走 callbackResultLabels,这里
// 只列 /bind/* handler。
func bindEndpointLabels() []string {
	return []string{"info", "verify_password", "otp_send", "otp_check", "confirm", "create"}
}

// bindResultLabels handler 的统一结果维度。与 callbackResultLabels 对应,
// 失败原因够粗以方便 dashboard 切分,但又能区分 token 失效 vs 限流 vs 内部异常。
func bindResultLabels() []string {
	return []string{
		"ok",
		"bad_request",          // 400:入参校验失败
		"unauthorized",         // 401:密码/OTP 错;状态机不是 verified
		"not_found",            // 410:token 已过期/未知
		"rate_limited",         // 429
		"conflict",             // 409:already_bound / status conflict
		"conflict_need_manual", // 409:manual_conflict 来源 token 拒建号(B. P2-1)
		"claims_incomplete",    // 422:/bind/create claims 缺 verified email/phone
		"internal_error",       // 500
		"not_ready",            // 503:Discovery 失败,bind service 未构造
	}
}

// init 把每个声明的 label 都预热成 0 值序列。Prometheus 在没观察到样本前不会
// 暴露 series,导致 Grafana"区分不出零次"和"未注册"两种状态。
func init() {
	for _, l := range callbackResultLabels() {
		metricCallbackTotal.WithLabelValues(l).Add(0)
	}
	for _, l := range stateConsumeResultLabels() {
		metricStateConsumeTotal.WithLabelValues(l).Add(0)
	}
	for _, l := range logoutResultLabels() {
		metricLogoutTotal.WithLabelValues(l).Add(0)
	}
	for _, l := range syncTickResultLabels() {
		metricSyncTickTotal.WithLabelValues(l).Add(0)
	}
	for _, l := range syncProcessedResultLabels() {
		metricSyncProcessedTotal.WithLabelValues(l).Add(0)
	}
	for _, l := range syncVerificationSyncedResultLabels() {
		metricSyncVerificationSyncedTotal.WithLabelValues(l).Add(0)
	}
	// 自助绑定:6 端点 × 10 结果 = 60 个序列,Prometheus 内存可忽略。
	for _, ep := range bindEndpointLabels() {
		for _, r := range bindResultLabels() {
			metricBindRequestTotal.WithLabelValues(ep, r).Add(0)
		}
	}
}

var (
	metricAuthorizeTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "authorize_total",
		Help:      "Total number of /authorize requests entering the OIDC handler.",
	})

	metricCallbackTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "callback_total",
		Help:      "Total number of /callback requests by terminal result.",
	}, []string{"result"})

	metricCallbackDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: metricNamespace,
		Name:      "callback_duration_seconds",
		Help:      "End-to-end /callback handler latency in seconds.",
		Buckets:   []float64{.05, .1, .25, .5, 1, 2, 5},
	})

	metricStateConsumeTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "state_consume_total",
		Help:      "OIDC state-store consume outcomes (ok|miss). miss includes both expired and never-existed.",
	}, []string{"result"})

	metricLogoutTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "logout_total",
		Help:      "POST /logout outcomes (ok|kick_fail|revoke_fail).",
	}, []string{"result"})

	metricSyncTickTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "sync_tick_total",
		Help:      "SyncWorker tick outcomes (ran|lock_held|lock_err).",
	}, []string{"result"})

	metricSyncProcessedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "sync_processed_total",
		Help:      "SyncWorker per-RT processing outcomes (ok|invalid_grant|transient|panic).",
	}, []string{"result"})

	// metricSyncVerificationSyncedTotal SyncWorker 在 rotate 成功后调 /userinfo
	// → UpsertVerificationFromOIDC 的出口分布(YUJ-405)。用于运维观察实名同步
	// 命中率 + 排查 Aegis /userinfo 异常。
	metricSyncVerificationSyncedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "sync_verification_synced_total",
		Help:      "SyncWorker verification sync outcomes after RT rotate (upserted|skipped_unverified|fetch_failed|upsert_failed).",
	}, []string{"status"})

	// metricBindRequestTotal /bind/* handler 的调用 + 结果分布。endpoint 列
	// 取自 bindEndpointLabels(),result 列取自 bindResultLabels()。
	// 注:callback 接管端点的"bind_pending"分支已归入 callback_total{result=bind_pending},
	// 不再这里重复。
	metricBindRequestTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "bind_request_total",
		Help:      "OIDC self-service bind handler outcomes by endpoint (info|verify_password|otp_send|otp_check|confirm|create) and result (ok|bad_request|unauthorized|not_found|rate_limited|conflict|conflict_need_manual|claims_incomplete|internal_error|not_ready).",
	}, []string{"endpoint", "result"})

	metricBindRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: metricNamespace,
		Name:      "bind_request_duration_seconds",
		Help:      "End-to-end OIDC self-service bind handler latency in seconds.",
		Buckets:   []float64{.02, .05, .1, .25, .5, 1, 2},
	}, []string{"endpoint"})
)
