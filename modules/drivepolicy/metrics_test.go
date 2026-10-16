package drivepolicy

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

// gatherPolicyVersion returns every sample of
// octo_drive_upload_policy_version_info currently exported by the collector the
// handler writes to, keyed by the policy_version label.
func gatherPolicyVersion(t *testing.T, reg prometheus.Gatherer) map[string]float64 {
	t.Helper()
	families, err := reg.Gather()
	require.NoError(t, err)

	out := map[string]float64{}
	for _, mf := range families {
		if mf.GetName() != "octo_drive_upload_policy_version_info" {
			continue
		}
		for _, m := range mf.GetMetric() {
			version := ""
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "policy_version" {
					version = lp.GetValue()
				}
			}
			out[version] = m.GetGauge().GetValue()
		}
	}
	return out
}

// A→B 回归：先后服务版本 A、B 后，导出必须只剩当前版本 B 一条 series。
// 这条用例钉住 reviewer 报告的缺陷 —— GaugeVec 形态下 A、B 会长期并存且都=1，
// 「当前版本」不可答、series 无限累积。
func TestPolicyVersionInfo_OnlyCurrentVersionIsExported(t *testing.T) {
	collector := newPolicyVersionCollector()
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(collector))

	// 首次服务前：不得有任何 series（避免把「尚未服务」误读成一个版本）。
	require.Empty(t, gatherPolicyVersion(t, reg), "首次成功下发前不应导出任何 series")

	collector.Set("aaaaaaaaaaaa")
	got := gatherPolicyVersion(t, reg)
	require.Equal(t, map[string]float64{"aaaaaaaaaaaa": 1}, got, "服务版本 A 后只剩 A")

	collector.Set("bbbbbbbbbbbb")
	got = gatherPolicyVersion(t, reg)
	require.Equal(t, map[string]float64{"bbbbbbbbbbbb": 1}, got,
		"服务版本 B 后必须只剩 B：旧版本 A 的 series 必须被回收，不能 A、B 并存")
	require.Len(t, got, 1, "任何时刻 policy_version series 恰为 1 条")
}

// 生产注册路径的自证：包级 collector 确实注册进了默认 Registry（/metrics 抓取
// 的那个），因此上面的语义就是线上语义。
func TestPolicyVersionInfo_RegisteredInDefaultRegistry(t *testing.T) {
	policyVersionInfo.Set("cccccccccccc")
	got := gatherPolicyVersion(t, prometheus.DefaultGatherer)
	require.Equal(t, map[string]float64{"cccccccccccc": 1}, got)
}

// 并发 Set 与 Gather 不产生数据竞争（-race 下有效）：collector 用 RWMutex 保护
// 单个 fingerprint，替换是同步的。
func TestPolicyVersionInfo_ConcurrentSetAndGather(t *testing.T) {
	collector := newPolicyVersionCollector()
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(collector))

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			collector.Set("v" + string(rune('a'+i%26)) + "00000000000")
			_, _ = reg.Gather()
		}
	}()
	for i := 0; i < 200; i++ {
		_, _ = reg.Gather()
	}
	<-done
}
