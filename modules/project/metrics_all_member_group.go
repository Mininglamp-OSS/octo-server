package project

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// 全员群相关的指标（P2）。
//
// 与 metrics.go 同一条纪律：没有 space_id / project_id / uid 标签，那些是无界的。
//
// 为什么建群失败和入群失败是**两个**计数器而不是一个带 kind 标签的：它们的运维
// 含义不同。建群失败意味着某个项目根本没有全员群——功能对这个项目是缺失的；入群
// 失败意味着群在、少了个人——功能在，数据有洞。前者要人去看为什么建不出来，后者
// 大概率是一次 IM 抖动，下一次加人就自愈了。合成一个指标会让这两条告警共用一个
// 阈值，而它们不该共用。

// 建群失败的原因。低基数枚举。
const (
	reasonProvisionerMissing       = "provisioner_missing"
	reasonProvisionClaimFailed     = "claim_failed"
	reasonProvisionCallFailed      = "provision_failed"
	reasonProvisionWriteBackFailed = "write_back_failed"
	reasonProvisionRaceLost        = "race_lost"
)

// 入群失败的原因。
const (
	reasonAdmitterMissing   = "admitter_missing"
	reasonAdmitLookupFailed = "lookup_failed"
	reasonAdmitCallFailed   = "admit_failed"
	// reasonAdmitSkippedNoGroup：本次写路径要入群，但这个项目此刻没有全员群。
	//
	// 单独一个原因而不是不计数，是 PR #855 第二轮 review 的 Q4：这一批人的入群被
	// 整批丢掉了，而丢掉它的那条分支原本什么都不记，理由是"补建那一路已经记过日志"。
	// 那条理由在补建**被跳过**（另一个写路径握着租约）时不成立——没跑，就没记。
	//
	// 名字按它真正度量的东西取，不按其中一种成因取。第四轮 review 的 Q7：叫
	// provision_race_lost 会在仪表盘上读作并发信号，而它同样会在补建**失败**时增长，
	// 与 provision_failures_total 重复计数一次——而这条分支分辨不出是哪一种。
	// "入群被跳过，因为没有群"才是它确实知道的事。
	reasonAdmitSkippedNoGroup = "admit_skipped_no_group"
)

// 同步失败的种类。
const (
	reasonSyncOwner = "owner"
	reasonSyncName  = "name"
)

var (
	// allMemberGroupProvisionFailures 计数"这个项目没能拿到全员群"。
	//
	// 它与 I4 扫描 A 的 gauge 是**互补**而不是重复：这个计数器记录的是失败的
	// 瞬间（可以定位到日志、到那一次请求），gauge 记录的是当下还有多少个项目
	// 处在没有群的状态（补建成功后会归零）。只有 gauge 的话，一次失败随后被
	// 补建成功，就在监控上不留任何痕迹——而那正是需要知道的事。
	allMemberGroupProvisionFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "all_member_group_provision_failures_total",
		Help: "Failed attempts to provision a project's all-member group, by reason. " +
			"A project with no all-member group is missing the feature, not just a row.",
	}, []string{"reason"})

	// allMemberGroupAdmitFailures 计数"这个人没能进全员群"。
	allMemberGroupAdmitFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "all_member_group_admit_failures_total",
		Help: "Failed attempts to admit a project member into the all-member group, by reason. " +
			"The seat is committed; the group membership is not. Reconcile scan B reports the gap.",
	}, []string{"reason"})

	// allMemberGroupSyncFailures 计数群主/群名同步失败。
	allMemberGroupSyncFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "all_member_group_sync_failures_total",
		Help:      "Failed attempts to sync the all-member group's owner or name with the project, by kind.",
	}, []string{"kind"})

	// allMemberGroupMissing 是 I4 扫描 A 的 gauge：当下有多少活跃项目没有可用的
	// 全员群（列为空，或指向一个已解散 / 已不属于本项目的群）。
	//
	// 只在完整轮转后发布，与 P0/P1 的全部 gauge 同一条规矩：一次被截断的 tick
	// 只数了键空间的一部分，把那个数发出去等于报一个假的下降。
	allMemberGroupMissing = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: metricNamespace,
		Name:      "all_member_group_missing",
		Help: "Active projects with no usable all-member group (I4 scan A). " +
			"Published only after a complete rotation.",
	})

	// allMemberGroupMemberGaps 是 I4 扫描 B 的 gauge：当下有多少个"项目活跃成员
	// 不在全员群里"的对子。
	allMemberGroupMemberGaps = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: metricNamespace,
		Name:      "all_member_group_member_gaps",
		Help: "Active project members missing from their project's all-member group (I4 scan B). " +
			"Published only after a complete rotation.",
	})
)

func observeAllMemberGroupProvisionFailure(reason string) {
	allMemberGroupProvisionFailures.WithLabelValues(reason).Inc()
}

func observeAllMemberGroupAdmitFailure(reason string) {
	allMemberGroupAdmitFailures.WithLabelValues(reason).Inc()
}

func observeAllMemberGroupSyncFailure(kind string) {
	allMemberGroupSyncFailures.WithLabelValues(kind).Inc()
}
