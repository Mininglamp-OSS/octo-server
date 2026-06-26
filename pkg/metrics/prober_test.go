package metrics

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// pingHistSampleCount 在 reg 中找 octo_server_dependency_duration_seconds,
// 返回 dependency="ping" + 指定 backend/status 的样本计数。
func pingHistSampleCount(t *testing.T, reg *prometheus.Registry, backend, status string) uint64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "octo_server_dependency_duration_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			var dep, backendL, statusL string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "dependency":
					dep = l.GetValue()
				case "backend":
					backendL = l.GetValue()
				case "status":
					statusL = l.GetValue()
				}
			}
			if dep == DependencyPing && backendL == backend && statusL == status {
				return m.GetHistogram().GetSampleCount()
			}
		}
	}
	return 0
}

// TestPingProber_ObservesOKAndError 驱动一轮探测,断言成功/失败各记一次到
// 与业务依赖同一个直方图(dependency="ping")。
func TestPingProber_ObservesOKAndError(t *testing.T) {
	reg := prometheus.NewRegistry()
	NewDependencyMetrics(reg) // 注册并登记为包级默认,供 ObservePing 使用

	probe := NewPingProber(time.Hour, time.Second). // interval 设大,只手动驱动一轮
							Add("mysql", func(context.Context) error { return nil }).
							Add("redis_ratelimit", func(context.Context) error { return errors.New("boom") })

	probe.probeAll(context.Background())

	if got := pingHistSampleCount(t, reg, "mysql", "ok"); got != 1 {
		t.Errorf("mysql ok sample count = %d, want 1", got)
	}
	if got := pingHistSampleCount(t, reg, "redis_ratelimit", "error"); got != 1 {
		t.Errorf("redis_ratelimit error sample count = %d, want 1", got)
	}
}

// TestPingProber_PassesTimeoutContext 断言探测函数拿到的是带 deadline 的 ctx
// (单次探测套了超时,避免卡死拖垮循环)。
func TestPingProber_PassesTimeoutContext(t *testing.T) {
	reg := prometheus.NewRegistry()
	NewDependencyMetrics(reg)

	var hadDeadline bool
	NewPingProber(time.Hour, 50*time.Millisecond).
		Add("mysql", func(c context.Context) error {
			_, hadDeadline = c.Deadline()
			return nil
		}).
		probeAll(context.Background())

	if !hadDeadline {
		t.Error("probe ctx 应带 deadline(单次探测超时),实际没有")
	}
}

// TestObservePing_NoDefaultIsNoop 未初始化默认 DependencyMetrics 时 ObservePing
// 必须是安全 no-op,不 panic。
func TestObservePing_NoDefaultIsNoop(t *testing.T) {
	defaultDependencyMetrics.Store(nil)
	ObservePing("mysql", time.Now(), nil) // 不 panic 即通过
}

// TestPingProber_StartNoTargetsNoGoroutine 无 target 时 Start 不应起 goroutine
// (退化为安全空操作)。这里只验证调用不 panic、可立即返回。
func TestPingProber_StartNoTargetsNoGoroutine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	NewPingProber(time.Hour, time.Second).Start(ctx) // 无 target
}
