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
	// reasonAdmitSubscribeFailed：group_member 行已经提交，broker 订阅失败。
	//
	// 与 admit_failed 分开，因为两者的见证者不同：准入事务失败留下的是"有项目席位、
	// 没有群成员行"，I4 扫描 B 会报；这一种行在、订阅缺，扫描 B 结构上看不见，而
	// 谁也读不回 broker 的订阅表（open_verification 里那一条）。合并计数会让告警
	// 把 on-call 指向一个对这个状态失明的扫描。PR #855 第七轮 review 的 P2-3。
	reasonAdmitSubscribeFailed = "subscribe_failed_after_commit"
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

	// allMemberGroupRosterTruncated 计数"补建时名册被截断"。
	//
	// 截断是可达的：updateProject 接受 max_members 且不校验当前活跃席位数，把配额
	// 调到低于现有成员数之后，每一次补建都只会带进前 max_members 个人，其余的人
	// 由 I4 扫描 B 报成缺口。
	//
	// 只有日志不够，而这不是"多加个指标更好"的问题：PR #855 第五轮 review 用变异
	// 测试证明，把名册查询的 +1 探针行拿掉之后（那正是上一轮的缺陷），
	// TestRebuildWarnsWhenTheRosterIsTruncated 仍然全绿——因为它断言的是种子的上界，
	// 而上界在两种写法下相同，唯一的差别就是这个信号发不发。一个只存在于日志里的
	// 信号，测试断言不了，于是它被删掉过一次、还能再被删一次。
	allMemberGroupRosterTruncated = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "all_member_group_roster_truncated_total",
		Help: "Rebuilds whose seed roster was cut at the project's max_members. " +
			"The members beyond the cut are not in the group; reconcile scan B reports them.",
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

// observeAllMemberGroupRosterTruncated 记录一次补建时的名册截断。
//
// reason 目前只有一个取值（over_max_members），保留标签是因为下一个截断来源
// （比如为了限制同步耗时而加的硬上限）应该分得开，而不是共用一个数字。
func observeAllMemberGroupRosterTruncated(reason string) {
	allMemberGroupRosterTruncated.WithLabelValues(reason).Inc()
}

// reasonTruncatedOverMaxMembers：活跃成员数超过项目的 max_members。
const reasonTruncatedOverMaxMembers = "over_max_members"

// allMemberGroupConvergenceIncomplete 计 Space 级联的群主收敛「这一次没走完」。
//
// 单独一个指标，不是 all_member_group_sync_failures_total 的一个 kind——那个计数器的
// Help 说的是"同步失败的次数"，而这件事**不是失败**：已经走到的项目都收敛了，只是这个
// 成员的历史项目行多到一次工单装不下。混进去，任何按名字对 kind 求和的告警都会因为一件
// 正常事件而呼人，而分辨它是不是失败恰恰需要先知道有这么个 kind——那就白设告警了。
// 第十一轮 fast-follow 的自审。
var allMemberGroupConvergenceIncomplete = promauto.NewCounter(prometheus.CounterOpts{
	Namespace: metricNamespace,
	Name:      "all_member_group_owner_convergence_incomplete_total",
	Help: "Space-removal owner convergences that spent their page budget with projects " +
		"left unvisited. Not a failure: the projects reached were converged, and the rest " +
		"are picked up by their own next membership change.",
})

// observeAllMemberGroupConvergenceIncomplete 记一次没走完的收敛。
func observeAllMemberGroupConvergenceIncomplete() {
	allMemberGroupConvergenceIncomplete.Inc()
}

func observeAllMemberGroupSyncFailure(kind string) {
	allMemberGroupSyncFailures.WithLabelValues(kind).Inc()
}
