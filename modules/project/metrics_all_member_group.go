package project

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Initial all-member-group lifecycle metrics. Member writes do not synchronize
// native group membership; only initial provisioning and project renames use
// these hooks.
const (
	reasonProvisionerMissing       = "provisioner_missing"
	reasonProvisionClaimFailed     = "claim_failed"
	reasonProvisionCallFailed      = "provision_failed"
	reasonProvisionWriteBackFailed = "write_back_failed"
	reasonProvisionRaceLost        = "race_lost"
	reasonSyncName                 = "name"
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

	// allMemberGroupSyncFailures counts failed project-name renames.
	allMemberGroupSyncFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "all_member_group_sync_failures_total",
		Help:      "Failed attempts to sync an all-member group's name with the project.",
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

func observeAllMemberGroupSyncFailure(kind string) {
	allMemberGroupSyncFailures.WithLabelValues(kind).Inc()
}
