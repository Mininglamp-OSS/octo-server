package metrics

import (
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// counterValue 在 reg 中按名找一个无 label(或单 label 匹配)的 counter 值。
func counterValue(t *testing.T, reg *prometheus.Registry, name, labelName, labelVal string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if labelName == "" {
				return m.GetCounter().GetValue()
			}
			for _, l := range m.GetLabel() {
				if l.GetName() == labelName && l.GetValue() == labelVal {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

func TestAvatarMetrics_RegisterAndObserve(t *testing.T) {
	prev := defaultAvatarMetrics.Load()
	t.Cleanup(func() { defaultAvatarMetrics.Store(prev) })

	reg := prometheus.NewRegistry()
	NewAvatarMetrics(reg) // 同时登记为包级默认

	ObserveAvatarCacheHit()
	ObserveAvatarCacheHit()
	ObserveAvatarCacheMiss()
	ObserveAvatarSingleflightShared()
	ObserveAvatarNotModified()
	ObserveAvatarRender(8*time.Millisecond, nil)
	ObserveAvatarRender(3*time.Millisecond, errors.New("boom"))
	ObserveAvatarSemaphoreWait(2 * time.Millisecond)
	AddAvatarRenderInflight(1)
	AddAvatarRenderInflight(-1)

	if got := counterValue(t, reg, "dmwork_avatar_cache_events_total", "result", "hit"); got != 2 {
		t.Fatalf("cache hit = %v, want 2", got)
	}
	if got := counterValue(t, reg, "dmwork_avatar_cache_events_total", "result", "miss"); got != 1 {
		t.Fatalf("cache miss = %v, want 1", got)
	}
	if got := counterValue(t, reg, "dmwork_avatar_render_singleflight_shared_total", "", ""); got != 1 {
		t.Fatalf("singleflight shared = %v, want 1", got)
	}
	if got := counterValue(t, reg, "dmwork_avatar_not_modified_total", "", ""); got != 1 {
		t.Fatalf("not_modified = %v, want 1", got)
	}

	// 渲染直方图按 status 各记一次。
	mfs, _ := reg.Gather()
	var okN, errN uint64
	for _, mf := range mfs {
		if mf.GetName() != "dmwork_avatar_render_duration_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "status" {
					switch l.GetValue() {
					case dependencyStatusOK:
						okN = m.GetHistogram().GetSampleCount()
					case dependencyStatusError:
						errN = m.GetHistogram().GetSampleCount()
					}
				}
			}
		}
	}
	if okN != 1 || errN != 1 {
		t.Fatalf("render duration samples ok=%d err=%d, want 1/1", okN, errN)
	}
}

func TestAvatarObserve_NoDefaultIsNoop(t *testing.T) {
	prev := defaultAvatarMetrics.Load()
	t.Cleanup(func() { defaultAvatarMetrics.Store(prev) })
	defaultAvatarMetrics.Store(nil)
	// 未初始化默认实例时必须是纯 no-op,不得 panic。
	ObserveAvatarCacheHit()
	ObserveAvatarCacheMiss()
	ObserveAvatarSingleflightShared()
	ObserveAvatarNotModified()
	ObserveAvatarRender(time.Millisecond, nil)
	ObserveAvatarSemaphoreWait(time.Millisecond)
	AddAvatarRenderInflight(1)
}
