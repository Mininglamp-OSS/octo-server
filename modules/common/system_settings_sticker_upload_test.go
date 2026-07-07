package common

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
)

// 自定义贴纸上传限制 + 服务端压缩配置读侧测试（sticker-upload-compression 任务）。
//
// 全部走 no-infra 模式：直接向 SystemSettings.snapshot 灌 map，规避 MySQL/Redis
// 依赖，与 TestSystemSettings_IncomingWebhook_ReadSideClamp_NoInfra 同风格。DB
// 全链路的 Load/Reload 由 CI 上其余 system_settings_test.go 用例覆盖。

// stickerSnapSettings builds a SystemSettings whose snapshot is exactly `snap`
// — used by every no-infra test in this file so the read-side clamp logic can be
// exercised without spinning up a test server.
func stickerSnapSettings(snap map[string]string) *SystemSettings {
	s := &SystemSettings{}
	m := map[string]string{}
	for k, v := range snap {
		m[k] = v
	}
	s.snapshot.Store(&m)
	return s
}

// ----- upload_max_size_kb -----

func TestSystemSettings_StickerUploadMaxSizeKB_DefaultsWhenUnset(t *testing.T) {
	s := stickerSnapSettings(nil)
	assert.Equal(t, defaultStickerUploadMaxSizeKB, s.StickerUploadMaxSizeKB())
}

func TestSystemSettings_StickerUploadMaxSizeKB_DBOverride(t *testing.T) {
	s := stickerSnapSettings(map[string]string{
		"sticker.upload_max_size_kb": "2048",
	})
	assert.Equal(t, 2048, s.StickerUploadMaxSizeKB())
}

// A DB row above the server-side hard cap clamps to the hard cap, NOT the input;
// otherwise a bad admin edit could exceed the resource envelope the hard cap is
// meant to protect (task brief: "配置需有服务端硬上限").
func TestSystemSettings_StickerUploadMaxSizeKB_ClampsToHardCap(t *testing.T) {
	s := stickerSnapSettings(map[string]string{
		"sticker.upload_max_size_kb": "999999",
	})
	assert.Equal(t, stickerUploadMaxSizeKBHardCap, s.StickerUploadMaxSizeKB())
}

// Non-positive / non-numeric values fall back to the default rather than being
// served verbatim (0 would "dark-close" the upload; negative would wrap around
// after multiplication to bytes). Matches the pattern in IncomingWebhookMaxPerGroup.
func TestSystemSettings_StickerUploadMaxSizeKB_ClampsNonPositive(t *testing.T) {
	for _, bad := range []string{"0", "-1", "abc", ""} {
		s := stickerSnapSettings(map[string]string{
			"sticker.upload_max_size_kb": bad,
		})
		assert.Equalf(t, defaultStickerUploadMaxSizeKB, s.StickerUploadMaxSizeKB(),
			"value=%q must fall back to default", bad)
	}
}

// The hard-cap value itself is accepted as-is (upper boundary is inclusive).
func TestSystemSettings_StickerUploadMaxSizeKB_HardCapBoundaryAccepted(t *testing.T) {
	s := stickerSnapSettings(map[string]string{
		"sticker.upload_max_size_kb": "5120",
	})
	assert.Equal(t, 5120, s.StickerUploadMaxSizeKB())
}

// ----- upload_max_dimension -----

func TestSystemSettings_StickerUploadMaxDimension_DefaultsWhenUnset(t *testing.T) {
	s := stickerSnapSettings(nil)
	assert.Equal(t, defaultStickerUploadMaxDimension, s.StickerUploadMaxDimension())
}

func TestSystemSettings_StickerUploadMaxDimension_DBOverride(t *testing.T) {
	s := stickerSnapSettings(map[string]string{
		"sticker.upload_max_dimension": "768",
	})
	assert.Equal(t, 768, s.StickerUploadMaxDimension())
}

func TestSystemSettings_StickerUploadMaxDimension_ClampsToHardCap(t *testing.T) {
	s := stickerSnapSettings(map[string]string{
		"sticker.upload_max_dimension": "8192",
	})
	assert.Equal(t, stickerUploadMaxDimensionHardCap, s.StickerUploadMaxDimension())
}

func TestSystemSettings_StickerUploadMaxDimension_ClampsNonPositive(t *testing.T) {
	for _, bad := range []string{"0", "-10", "abc"} {
		s := stickerSnapSettings(map[string]string{
			"sticker.upload_max_dimension": bad,
		})
		assert.Equalf(t, defaultStickerUploadMaxDimension, s.StickerUploadMaxDimension(),
			"value=%q must fall back to default", bad)
	}
}

// ----- upload_allowed_formats -----

func sortedExts(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

// 未配置 → 全部 5 种位图（复刻历史硬编码 stickerUploadExts）。
func TestSystemSettings_StickerUploadAllowedFormats_DefaultsFullSet(t *testing.T) {
	s := stickerSnapSettings(nil)
	got := s.StickerUploadAllowedFormats()
	assert.Equal(t,
		[]string{".gif", ".jpeg", ".jpg", ".png", ".webp"},
		sortedExts(got),
	)
}

// 配置只能收窄（narrow），得到的是配置和位图白名单的交集。
func TestSystemSettings_StickerUploadAllowedFormats_NarrowsToConfig(t *testing.T) {
	s := stickerSnapSettings(map[string]string{
		"sticker.upload_allowed_formats": "png,jpg",
	})
	assert.Equal(t, []string{".jpg", ".png"}, sortedExts(s.StickerUploadAllowedFormats()))
}

// 位图白名单外的类型（mp4/pdf/svg）在读侧被丢弃，绝不放开。这条是硬上限一部分：
// 即使运营写错，也不会让非位图作为贴纸被存进 sticker/ keyspace。
func TestSystemSettings_StickerUploadAllowedFormats_DropsNonRaster(t *testing.T) {
	s := stickerSnapSettings(map[string]string{
		"sticker.upload_allowed_formats": "png,mp4,pdf,svg",
	})
	assert.Equal(t, []string{".png"}, sortedExts(s.StickerUploadAllowedFormats()))
}

// 大小写、前后空格、前缀点号缺失都归一化。
func TestSystemSettings_StickerUploadAllowedFormats_Normalizes(t *testing.T) {
	s := stickerSnapSettings(map[string]string{
		"sticker.upload_allowed_formats": "  PNG  ,  .JPG  , .Gif ",
	})
	assert.Equal(t, []string{".gif", ".jpg", ".png"}, sortedExts(s.StickerUploadAllowedFormats()))
}

// 配置存在但交集为空（全非法）→ 回退默认全 5 种，避免"运营写错就把功能暗关"。
func TestSystemSettings_StickerUploadAllowedFormats_EmptyIntersectionFallsBack(t *testing.T) {
	for _, bad := range []string{"", " ", ",,,", "mp4,pdf"} {
		s := stickerSnapSettings(map[string]string{
			"sticker.upload_allowed_formats": bad,
		})
		got := s.StickerUploadAllowedFormats()
		assert.Equalf(t,
			[]string{".gif", ".jpeg", ".jpg", ".png", ".webp"},
			sortedExts(got),
			"value=%q must fall back to the full default set", bad,
		)
	}
}

// ----- compress_enabled -----

func TestSystemSettings_StickerCompressEnabled_DefaultsFalse(t *testing.T) {
	s := stickerSnapSettings(nil)
	assert.False(t, s.StickerCompressEnabled(), "DB empty -> compression off by default (grey-out required)")
}

func TestSystemSettings_StickerCompressEnabled_DBTrueWins(t *testing.T) {
	s := stickerSnapSettings(map[string]string{
		"sticker.compress_enabled": "1",
	})
	assert.True(t, s.StickerCompressEnabled())
}

// ----- compress_target_kb / max_concurrency / timeout_ms clamp -----

func TestSystemSettings_StickerCompressTargetKB_ClampBehavior(t *testing.T) {
	tests := map[string]int{
		"":       defaultStickerCompressTargetKB,      // unset → default
		"0":      defaultStickerCompressTargetKB,      // ≤0 → default
		"-1":     defaultStickerCompressTargetKB,      // negative → default
		"abc":    defaultStickerCompressTargetKB,      // non-numeric → default
		"999999": stickerCompressTargetKBHardCap,      // over hard cap → hard cap
		"512":    512,                                 // in-range → verbatim
		"5120":   stickerCompressTargetKBHardCap,      // exact hard cap → accepted
	}
	for in, want := range tests {
		s := stickerSnapSettings(map[string]string{
			"sticker.compress_target_kb": in,
		})
		assert.Equalf(t, want, s.StickerCompressTargetKB(), "target_kb=%q", in)
	}
}

func TestSystemSettings_StickerCompressMaxConcurrency_ClampBehavior(t *testing.T) {
	tests := map[string]int{
		"":       defaultStickerCompressMaxConcurrency,
		"0":      defaultStickerCompressMaxConcurrency,
		"-2":     defaultStickerCompressMaxConcurrency,
		"abc":    defaultStickerCompressMaxConcurrency,
		"999":    stickerCompressMaxConcurrencyHardCap,
		"8":      8,
		"32":     stickerCompressMaxConcurrencyHardCap,
	}
	for in, want := range tests {
		s := stickerSnapSettings(map[string]string{
			"sticker.compress_max_concurrency": in,
		})
		assert.Equalf(t, want, s.StickerCompressMaxConcurrency(), "max_concurrency=%q", in)
	}
}

func TestSystemSettings_StickerCompressTimeoutMs_ClampBehavior(t *testing.T) {
	tests := map[string]int{
		"":        defaultStickerCompressTimeoutMs,
		"0":       defaultStickerCompressTimeoutMs,
		"-100":    defaultStickerCompressTimeoutMs,
		"abc":     defaultStickerCompressTimeoutMs,
		"9999999": stickerCompressTimeoutMsHardCap,
		"500":     500,
		"10000":   stickerCompressTimeoutMsHardCap,
	}
	for in, want := range tests {
		s := stickerSnapSettings(map[string]string{
			"sticker.compress_timeout_ms": in,
		})
		assert.Equalf(t, want, s.StickerCompressTimeoutMs(), "timeout_ms=%q", in)
	}
}
