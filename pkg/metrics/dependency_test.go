package metrics

import (
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// histSampleCount 在 reg 中找 dmwork_dependency_duration_seconds,返回带指定
// status label 的 histogram 的观测次数(SampleCount)。找不到返回 0。
func histSampleCount(t *testing.T, reg *prometheus.Registry, status string) uint64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "dmwork_dependency_duration_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "status" && l.GetValue() == status {
					return m.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	return 0
}

func TestDependencyMetrics_ObserveOKAndError(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewDependencyMetrics(reg)

	m.Observe(DependencyObjectStore, OpGetFile, "minio", time.Now(), nil)
	m.Observe(DependencyObjectStore, OpGetFile, "minio", time.Now(), errors.New("boom"))

	if got := histSampleCount(t, reg, dependencyStatusOK); got != 1 {
		t.Fatalf("ok sample count = %d, want 1", got)
	}
	if got := histSampleCount(t, reg, dependencyStatusError); got != 1 {
		t.Fatalf("error sample count = %d, want 1", got)
	}
}

func TestObserveObjectStore_NoDefaultIsNoop(t *testing.T) {
	// 快照并在结束时恢复包级默认,避免污染同一 run 内后续测试(#442 P2-1)。
	prev := defaultDependencyMetrics.Load()
	t.Cleanup(func() { defaultDependencyMetrics.Store(prev) })
	// 重置包级默认,模拟"未初始化"(指标关闭 / 进程未注册)场景。
	defaultDependencyMetrics.Store(nil)
	// 不应 panic,纯 no-op。
	ObserveObjectStore(OpGetFile, "minio", time.Now(), nil)
}

func TestObserveObjectStore_UsesDefault(t *testing.T) {
	prev := defaultDependencyMetrics.Load()
	t.Cleanup(func() { defaultDependencyMetrics.Store(prev) })
	reg := prometheus.NewRegistry()
	NewDependencyMetrics(reg) // 同时把自己设为包级默认

	ObserveObjectStore(OpUploadFile, "oss", time.Now(), nil)

	if got := histSampleCount(t, reg, dependencyStatusOK); got != 1 {
		t.Fatalf("default observer recorded %d ok samples, want 1", got)
	}
}

func TestDependencyMetrics_Naming(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewDependencyMetrics(reg)
	m.Observe(DependencyObjectStore, OpGetFile, "minio", time.Now(), nil)

	const want = "dmwork_dependency_duration_seconds"
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, mf := range mfs {
		if mf.GetName() == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("metric family %q not registered", want)
	}
}
