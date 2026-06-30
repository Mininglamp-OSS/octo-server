package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// gaugeValue 在 reg 中按名 + 单 label 找一个 gauge 值。
func gaugeValue(t *testing.T, reg *prometheus.Registry, name, labelName, labelVal string) float64 {
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
			for _, l := range m.GetLabel() {
				if l.GetName() == labelName && l.GetValue() == labelVal {
					return m.GetGauge().GetValue()
				}
			}
		}
	}
	return 0
}

func TestStickerMetrics_RegisterAndObserve(t *testing.T) {
	prev := defaultStickerMetrics.Load()
	t.Cleanup(func() { defaultStickerMetrics.Store(prev) })

	reg := prometheus.NewRegistry()
	NewStickerMetrics(reg) // 同时登记为包级默认

	// 预热后所有 result 序列应已存在且为 0。
	for _, r := range stickerRegisterResults() {
		if got := counterValue(t, reg, "dmwork_sticker_register_total", "result", r); got != 0 {
			t.Fatalf("warmup register{%s} = %v, want 0", r, got)
		}
	}

	ObserveStickerUploadHandleIssued()
	ObserveStickerUploadHandleIssued()
	ObserveStickerRegister(StickerRegisterOK)
	ObserveStickerRegister(StickerRegisterCompatMissing)
	ObserveStickerRegister(StickerRegisterCompatMissing)
	ObserveStickerRegister(StickerRegisterRejectedInvalid)
	SetStickerHandlePolicy(true, false)

	if got := counterValue(t, reg, "dmwork_sticker_upload_handle_issued_total", "", ""); got != 2 {
		t.Fatalf("upload_handle_issued = %v, want 2", got)
	}
	if got := counterValue(t, reg, "dmwork_sticker_register_total", "result", StickerRegisterOK); got != 1 {
		t.Fatalf("register{ok} = %v, want 1", got)
	}
	if got := counterValue(t, reg, "dmwork_sticker_register_total", "result", StickerRegisterCompatMissing); got != 2 {
		t.Fatalf("register{compat_missing} = %v, want 2", got)
	}
	if got := counterValue(t, reg, "dmwork_sticker_register_total", "result", StickerRegisterRejectedInvalid); got != 1 {
		t.Fatalf("register{rejected_invalid} = %v, want 1", got)
	}
	if got := gaugeValue(t, reg, "dmwork_sticker_handle_policy", "setting", "enabled"); got != 1 {
		t.Fatalf("handle_policy{enabled} = %v, want 1", got)
	}
	if got := gaugeValue(t, reg, "dmwork_sticker_handle_policy", "setting", "required"); got != 0 {
		t.Fatalf("handle_policy{required} = %v, want 0", got)
	}
}

func TestStickerObserve_NoDefaultIsNoop(t *testing.T) {
	prev := defaultStickerMetrics.Load()
	t.Cleanup(func() { defaultStickerMetrics.Store(prev) })
	defaultStickerMetrics.Store(nil)
	// 未初始化默认实例时必须是纯 no-op，不得 panic。
	ObserveStickerUploadHandleIssued()
	ObserveStickerRegister(StickerRegisterOK)
	SetStickerHandlePolicy(true, true)
}
