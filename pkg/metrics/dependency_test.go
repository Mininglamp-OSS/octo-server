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

// histSampleCountByDepOp 返回带指定 dependency+op label 的 histogram 观测次数,
// 跨 status 累加。dependency+op 下可能同时存在 status=ok 与 status=error 两条序列;
// 若只取首个匹配会按 protobuf 顺序漏数,故这里把所有匹配序列的 SampleCount 相加。
func histSampleCountByDepOp(t *testing.T, reg *prometheus.Registry, dependency, op string) uint64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var total uint64
	for _, mf := range mfs {
		if mf.GetName() != "dmwork_dependency_duration_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			var gotDep, gotOp string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "dependency":
					gotDep = l.GetValue()
				case "op":
					gotOp = l.GetValue()
				}
			}
			if gotDep == dependency && gotOp == op {
				total += m.GetHistogram().GetSampleCount()
			}
		}
	}
	return total
}

func TestDependencyMetrics_ObserveDuration(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewDependencyMetrics(reg)

	m.ObserveDuration(DependencyMySQL, "query", backendMain, 5*time.Millisecond, nil)
	m.ObserveDuration(DependencyMySQL, "connect", backendMain, 2*time.Millisecond, errors.New("dial fail"))

	if got := histSampleCountByDepOp(t, reg, DependencyMySQL, "query"); got != 1 {
		t.Fatalf("mysql query sample count = %d, want 1", got)
	}
	if got := histSampleCountByDepOp(t, reg, DependencyMySQL, "connect"); got != 1 {
		t.Fatalf("mysql connect sample count = %d, want 1", got)
	}
	if got := histSampleCount(t, reg, dependencyStatusError); got != 1 {
		t.Fatalf("error sample count = %d, want 1 (failed connect)", got)
	}
}

func TestObserveDBAndRedis_UsesDefault(t *testing.T) {
	prev := defaultDependencyMetrics.Load()
	t.Cleanup(func() { defaultDependencyMetrics.Store(prev) })
	reg := prometheus.NewRegistry()
	NewDependencyMetrics(reg) // 同时把自己设为包级默认

	ObserveDB("query", 3*time.Millisecond, nil)
	ObserveRedisCmd("get", time.Millisecond, nil)

	if got := histSampleCountByDepOp(t, reg, DependencyMySQL, "query"); got != 1 {
		t.Fatalf("ObserveDB recorded %d mysql/query samples, want 1", got)
	}
	if got := histSampleCountByDepOp(t, reg, DependencyRedis, "get"); got != 1 {
		t.Fatalf("ObserveRedisCmd recorded %d redis/get samples, want 1", got)
	}
}

func TestObserveDBAndRedis_NoDefaultIsNoop(t *testing.T) {
	prev := defaultDependencyMetrics.Load()
	t.Cleanup(func() { defaultDependencyMetrics.Store(prev) })
	defaultDependencyMetrics.Store(nil)
	// 未初始化默认实例时必须是纯 no-op,不得 panic。
	ObserveDB("query", time.Millisecond, nil)
	ObserveRedisCmd("set", time.Millisecond, errors.New("boom"))
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
