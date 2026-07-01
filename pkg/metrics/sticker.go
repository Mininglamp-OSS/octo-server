package metrics

import (
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
)

// 自定义贴纸 handle 上线观测指标（P0: Sticker Handle Enforcement Rollout）。
// #509 给贴纸上传加了签名句柄；本组指标支撑把「强制 handle」从隐式开关变成
// 可灰度、可回滚、可观测的上线：在切到强制（system_setting sticker.handle_required
// = true）之前，先观察老客户端「缺 handle」的注册占比是否归零。
//
// 跨模块共享：签发发生在 modules/file（上传成功后下发 sticker_handle），注册
// 校验发生在 modules/sticker。两者都依赖 leaf 包 pkg/metrics，故指标定义在此，
// 经包级 Observe 函数灌入，避免任一业务模块互相依赖。
//
// label 维度收窄到低基数枚举（result / setting），不放 uid 等高基数内容。

// 注册结果枚举（Register 的 result label）。任何新增分支都应回到这里加常量，
// 并在 NewStickerMetrics 预热里补一行，Grafana dashboard 才能稳定区分「零次」
// 与「序列不存在」。
const (
	// StickerRegisterOK 通过：路径形状校验过，且（有能力时）handle 校验通过，
	// 或裸跑无能力时路径形状校验过。
	StickerRegisterOK = "ok"
	// StickerRegisterCompatMissing 兼容模式（required=false）下缺 handle 被放行。
	// 这是上线观测的核心指标：其占比归零表示老客户端基本退场，可切 required=true。
	StickerRegisterCompatMissing = "compat_missing"
	// StickerRegisterRejectedMissing required=true 下缺 handle 被拒。
	StickerRegisterRejectedMissing = "rejected_missing"
	// StickerRegisterRejectedInvalid handle 非法/错配被拒（与 required 无关，恒拒）。
	StickerRegisterRejectedInvalid = "rejected_invalid"
	// StickerRegisterRejectedPath 路径形状校验失败被拒（先于 handle 判定）。
	StickerRegisterRejectedPath = "rejected_path"
	// StickerRegisterRejectedNoCapability 配置冲突被拒（fail-closed）：策略要求 handle
	// （sticker.handle_required=true）但服务端无有效 OCTO_MASTER_KEY 提供校验能力，无法
	// 兑现强制，故拒绝而非静默放行。这条与 ok 区分开，dashboard 才能看出「声称强制却
	// 因缺能力在异常拒绝」的 misconfig，而不是被计成正常成功掩盖。
	StickerRegisterRejectedNoCapability = "rejected_no_capability"
)

// stickerRegisterResults 是 Register 的全部 result 取值，用于启动预热成 0 值序列。
func stickerRegisterResults() []string {
	return []string{
		StickerRegisterOK,
		StickerRegisterCompatMissing,
		StickerRegisterRejectedMissing,
		StickerRegisterRejectedInvalid,
		StickerRegisterRejectedPath,
		StickerRegisterRejectedNoCapability,
	}
}

// StickerMetrics 持有自定义贴纸 handle 的上线观测指标。每进程一个实例，注册到
// 一个 Registerer。
type StickerMetrics struct {
	// UploadHandleIssued 计数 /v1/file/upload?type=sticker 成功并签发 sticker_handle
	// 的次数（仅在 Enabled 且签发成功时 +1）。
	UploadHandleIssued prometheus.Counter
	// Register 按 result 计数 POST /v1/sticker/user 的注册结果（见 StickerRegister* 常量）。
	Register *prometheus.CounterVec
	// HandlePolicy 反映启动时的部署姿态：setting=enabled（master key 能力）/
	// setting=required（强制策略）各置 0/1，便于 dashboard 确认配置并对
	// 「required 但无能力」的冲突态告警。
	HandlePolicy *prometheus.GaugeVec
}

// defaultStickerMetrics 是供包级 Observe 函数使用的进程默认实例。未设置时 Observe
// 为安全 no-op（同 avatar.go / dependency.go 的 atomic.Pointer 约定）。
var defaultStickerMetrics atomic.Pointer[StickerMetrics]

// NewStickerMetrics 在 reg 上注册贴纸 handle 指标，并登记为包级默认。
// 调用契约同 NewAvatarMetrics：同一 Registerer 只应调用一次。
func NewStickerMetrics(reg prometheus.Registerer) *StickerMetrics {
	m := &StickerMetrics{
		UploadHandleIssued: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: "sticker",
			Name:      "upload_handle_issued_total",
			Help:      "Custom-sticker uploads that succeeded and were issued a signed sticker_handle.",
		}),
		Register: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: "sticker",
			Name:      "register_total",
			Help:      "Custom-sticker registration outcomes (ok|compat_missing|rejected_missing|rejected_invalid|rejected_path|rejected_no_capability).",
		}, []string{"result"}),
		HandlePolicy: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: "sticker",
			Name:      "handle_policy",
			Help:      "Sticker handle deployment posture: setting=enabled (master-key capability) / setting=required (enforcement policy), each 0 or 1.",
		}, []string{"setting"}),
	}
	reg.MustRegister(m.UploadHandleIssued, m.Register, m.HandlePolicy)
	// 预热 result 序列为 0，dashboard 才能区分「零次」与「未注册」。
	for _, r := range stickerRegisterResults() {
		m.Register.WithLabelValues(r).Add(0)
	}
	// 同理预热 handle_policy 两个维度（enabled/required）为 0；虽然启动即被
	// SetStickerHandlePolicy 覆盖，但与上面 result 序列「零 vs 缺失」的一致性对齐，
	// 且避免 SetStickerHandlePolicy 因故未调用时该 gauge 完全缺席。
	m.HandlePolicy.WithLabelValues("enabled").Set(0)
	m.HandlePolicy.WithLabelValues("required").Set(0)
	defaultStickerMetrics.Store(m)
	return m
}

// ObserveStickerUploadHandleIssued 记录一次上传成功并签发 handle。
func ObserveStickerUploadHandleIssued() {
	if m := defaultStickerMetrics.Load(); m != nil {
		m.UploadHandleIssued.Inc()
	}
}

// ObserveStickerRegister 记录一次注册结果。result 应取 StickerRegister* 常量。
func ObserveStickerRegister(result string) {
	if m := defaultStickerMetrics.Load(); m != nil {
		m.Register.WithLabelValues(result).Inc()
	}
}

// SetStickerHandlePolicy 在启动时设置部署姿态 gauge（enabled = 是否有 master key
// 能力，required = 是否强制）。两者正交，便于 dashboard 对「required 但无能力」
// 的配置冲突告警。
func SetStickerHandlePolicy(enabled, required bool) {
	m := defaultStickerMetrics.Load()
	if m == nil {
		return
	}
	m.HandlePolicy.WithLabelValues("enabled").Set(boolToGauge(enabled))
	m.HandlePolicy.WithLabelValues("required").Set(boolToGauge(required))
}

func boolToGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
