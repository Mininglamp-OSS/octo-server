package common

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/stretchr/testify/assert"
)

// Drive 上传上限读侧测试（task drive-upload-policy / 设计 §7 M1）。
// 全部 no-infra：直接向 SystemSettings.snapshot 灌 map，与
// system_settings_file_upload_test.go 同风格。

func driveSnapSettings(snap map[string]string) *SystemSettings {
	s := &SystemSettings{Log: log.NewTLog("SystemSettingsTest")}
	m := map[string]string{}
	for k, v := range snap {
		m[k] = v
	}
	s.snapshot.Store(&m)
	return s
}

// ----- 天花板 env：OCTO_DRIVE_UPLOAD_MAX_SIZE_KB_HARD_CAP -----

func TestDriveUploadMaxSizeKBHardCap_DefaultsWhenUnset(t *testing.T) {
	t.Setenv(envDriveUploadMaxSizeKBHardCap, "")
	assert.Equal(t, defaultDriveUploadMaxSizeKBHardCap, DriveUploadMaxSizeKBHardCap())
}

func TestDriveUploadMaxSizeKBHardCap_FromEnv(t *testing.T) {
	t.Setenv(envDriveUploadMaxSizeKBHardCap, "5242880") // 5 GiB
	assert.Equal(t, 5242880, DriveUploadMaxSizeKBHardCap())
}

func TestDriveUploadMaxSizeKBHardCap_RejectsInvalid(t *testing.T) {
	for _, bad := range []string{"abc", "0", "-1", "1.5", "  ", "999999999999"} {
		t.Setenv(envDriveUploadMaxSizeKBHardCap, bad)
		assert.Equal(t, defaultDriveUploadMaxSizeKBHardCap, DriveUploadMaxSizeKBHardCap(),
			"env=%q 应回落默认天花板，不 panic、不暴露非法值", bad)
	}
}

func TestDriveUploadMaxSizeKBHardCap_From_NilGetenv(t *testing.T) {
	assert.Equal(t, defaultDriveUploadMaxSizeKBHardCap, driveUploadMaxSizeKBHardCapFrom(nil))
}

// ----- 生效值：DriveUploadMaxSizeKB -----

func TestDriveUploadMaxSizeKB(t *testing.T) {
	t.Setenv(envDriveUploadMaxSizeKBHardCap, "") // 默认天花板 10 GiB

	// 未配置 → 代码默认 1048576 (1 GiB)
	assert.Equal(t, DefaultDriveUploadMaxSizeKB, driveSnapSettings(nil).DriveUploadMaxSizeKB())
	assert.Equal(t, 1048576, DefaultDriveUploadMaxSizeKB)

	// 已配置且不超天花板 → 原样生效
	assert.Equal(t, 2097152, driveSnapSettings(map[string]string{"drive.upload_max_size_kb": "2097152"}).DriveUploadMaxSizeKB())

	// 非法值 → 回落默认
	for _, bad := range []string{"abc", "0", "-5", ""} {
		assert.Equal(t, DefaultDriveUploadMaxSizeKB,
			driveSnapSettings(map[string]string{"drive.upload_max_size_kb": bad}).DriveUploadMaxSizeKB(),
			"value=%q", bad)
	}

	// 等于天花板 → 不钳
	atCap := driveSnapSettings(map[string]string{"drive.upload_max_size_kb": "10485760"})
	assert.Equal(t, 10485760, atCap.DriveUploadMaxSizeKB())

	// 天花板 +1 → 钳到天花板（不是回落默认）
	over := driveSnapSettings(map[string]string{"drive.upload_max_size_kb": "10485761"})
	assert.Equal(t, DriveUploadMaxSizeKBHardCap(), over.DriveUploadMaxSizeKB())
	assert.Equal(t, 10485760, over.DriveUploadMaxSizeKB())
}

func TestDriveUploadMaxSizeKB_RespectsEnvHardCap(t *testing.T) {
	t.Setenv(envDriveUploadMaxSizeKBHardCap, "5242880") // 天花板 5 GiB
	s := driveSnapSettings(map[string]string{"drive.upload_max_size_kb": "4194304"})
	assert.Equal(t, 4194304, s.DriveUploadMaxSizeKB(), "未超天花板的值应原样生效")
}

// env 天花板可以低于代码默认值：未配置时生效值必须取 min(default, cap)，
// 而不是先拿 1048576 再钳（那会对外谎报一次没发生过的 admin 变更，并让部署
// 自己声明的天花板形同虚设）。这是 clamp 的 ≤0 分支专门为 file 修过的坑，
// drive 沿用同一形态。
func TestDriveUploadMaxSizeKB_UnconfiguredClampedByLoweredEnvHardCap(t *testing.T) {
	t.Setenv(envDriveUploadMaxSizeKBHardCap, "2048") // 2 MiB
	s := driveSnapSettings(nil)
	assert.Equal(t, 2048, s.DriveUploadMaxSizeKB(),
		"未配置时应取 min(default, cap)，不能超出部署声明的天花板")
}

func TestDriveUploadMaxSizeKB_ClampedByLoweredEnvHardCap(t *testing.T) {
	t.Setenv(envDriveUploadMaxSizeKBHardCap, "2048") // 天花板 2 MiB
	s := driveSnapSettings(map[string]string{"drive.upload_max_size_kb": "1048576"})
	assert.Equal(t, 2048, s.DriveUploadMaxSizeKB())
}

// 天花板 env 变更（需重启）会体现在生效值上：这是「env 要重启 → 本身是防线」的
// 可观测证据，也防止有人把天花板做成读一次就缓存的常量。
func TestDriveUploadMaxSizeKB_TracksHardCapEnv(t *testing.T) {
	s := driveSnapSettings(map[string]string{"drive.upload_max_size_kb": "1048576"})
	t.Setenv(envDriveUploadMaxSizeKBHardCap, "4096")
	assert.Equal(t, 4096, s.DriveUploadMaxSizeKB())
	t.Setenv(envDriveUploadMaxSizeKBHardCap, "10485760")
	assert.Equal(t, 1048576, s.DriveUploadMaxSizeKB())
}
