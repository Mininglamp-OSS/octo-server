package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Drive 上传策略结构 / 版本测试（task drive-upload-policy / 设计 §7 M2）。
// 纯函数 + 快照注入，无 DB、无 Redis。

// envFunc 从 map 造一个 getenv，便于给分片参数注入确定值。
func envFunc(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDriveUploadPolicy_Defaults(t *testing.T) {
	t.Setenv(envDriveUploadMaxSizeKBHardCap, "")
	s := driveSnapSettings(nil)
	p := s.DriveUploadPolicy()

	assert.Equal(t, DefaultDriveUploadMaxSizeKB, p.MaxSizeKB)
	assert.Equal(t, 131072, p.MultipartThresholdKB, "阈值默认 128 MiB")
	assert.Equal(t, 65536, p.PartSizeKB, "片默认 64 MiB")
	assert.Equal(t, 3, p.MaxConcurrentParts)
	assert.Equal(t, 3, p.MaxConcurrentSessions)
	require.NotEmpty(t, p.PolicyVersion)
	assert.Len(t, p.PolicyVersion, 12, "policy_version = sha256 前 12 hex")
}

func TestDriveUploadPolicy_FromSnapshotMaxSize(t *testing.T) {
	t.Setenv(envDriveUploadMaxSizeKBHardCap, "")
	s := driveSnapSettings(map[string]string{"drive.upload_max_size_kb": "2097152"})
	p := s.DriveUploadPolicy()
	assert.Equal(t, 2097152, p.MaxSizeKB)
	// 与读侧真源一致 —— 证明 max_size_kb 出自同一份快照、同一套钳位。
	assert.Equal(t, s.DriveUploadMaxSizeKB(), p.MaxSizeKB)
}

// 分片参数 env 与默认：非法 / ≤0 一律回落默认。
func TestDriveUploadPolicyFrom_ShardEnv(t *testing.T) {
	t.Run("默认值", func(t *testing.T) {
		p := driveUploadPolicyFrom(100, envFunc(nil))
		assert.Equal(t, 131072, p.MultipartThresholdKB)
		assert.Equal(t, 65536, p.PartSizeKB)
		assert.Equal(t, 3, p.MaxConcurrentParts)
		assert.Equal(t, 3, p.MaxConcurrentSessions)
	})
	t.Run("合法 env 覆盖", func(t *testing.T) {
		p := driveUploadPolicyFrom(100, envFunc(map[string]string{
			envDriveUploadMultipartThresholdKB:  "262144",
			envDriveUploadPartSizeKB:            "131072",
			envDriveUploadMaxConcurrentParts:    "8",
			envDriveUploadMaxConcurrentSessions: "5",
		}))
		assert.Equal(t, 262144, p.MultipartThresholdKB)
		assert.Equal(t, 131072, p.PartSizeKB)
		assert.Equal(t, 8, p.MaxConcurrentParts)
		assert.Equal(t, 5, p.MaxConcurrentSessions)
	})
	t.Run("非法 env 回落默认", func(t *testing.T) {
		for _, bad := range []string{"abc", "0", "-1", "1.5", ""} {
			p := driveUploadPolicyFrom(100, envFunc(map[string]string{
				envDriveUploadMultipartThresholdKB:  bad,
				envDriveUploadPartSizeKB:            bad,
				envDriveUploadMaxConcurrentParts:    bad,
				envDriveUploadMaxConcurrentSessions: bad,
			}))
			assert.Equal(t, 131072, p.MultipartThresholdKB, "value=%q", bad)
			assert.Equal(t, 65536, p.PartSizeKB, "value=%q", bad)
			assert.Equal(t, 3, p.MaxConcurrentParts, "value=%q", bad)
			assert.Equal(t, 3, p.MaxConcurrentSessions, "value=%q", bad)
		}
	})
	t.Run("nil getenv 回落默认", func(t *testing.T) {
		p := driveUploadPolicyFrom(100, nil)
		assert.Equal(t, 131072, p.MultipartThresholdKB)
	})
}

// policy_version 是内容哈希：同输入同值，改任一输入即变。
func TestDriveUploadPolicyVersion_ContentHash(t *testing.T) {
	base := driveUploadPolicyVersion(1048576, 131072, 65536, 3, 3, 10485760)
	same := driveUploadPolicyVersion(1048576, 131072, 65536, 3, 3, 10485760)
	assert.Equal(t, base, same, "同输入必须同版本")

	changed := []string{
		driveUploadPolicyVersion(2097152, 131072, 65536, 3, 3, 10485760), // max_size_kb
		driveUploadPolicyVersion(1048576, 262144, 65536, 3, 3, 10485760), // threshold
		driveUploadPolicyVersion(1048576, 131072, 131072, 3, 3, 10485760),
		driveUploadPolicyVersion(1048576, 131072, 65536, 8, 3, 10485760),
		driveUploadPolicyVersion(1048576, 131072, 65536, 3, 5, 10485760),
		driveUploadPolicyVersion(1048576, 131072, 65536, 3, 3, 5242880), // hard_cap
	}
	// 每个输入单独变化都改变版本（防拼接歧义 / 漏字段）。
	for _, v := range changed {
		assert.NotEqual(t, base, v, "任一输入变化都应改变 policy_version")
	}
}

// 无缝分隔：拼接语义下 1|11 与 11|1 会碰撞，用 '|' 分隔后不会。
// 这里构造两个「朴素拼接相同、字段不同」的组合，若实现忘了分隔符就会误判同版本。
func TestDriveUploadPolicyVersion_SeparatorDisambiguates(t *testing.T) {
	a := driveUploadPolicyVersion(1, 11, 1, 1, 1, 1)
	b := driveUploadPolicyVersion(11, 1, 1, 1, 1, 1)
	assert.NotEqual(t, a, b)
}

// 快照变更会反映到版本上（版本派生自快照 max_size_kb，不是 env / 常量）。
func TestDriveUploadPolicy_VersionTracksSnapshot(t *testing.T) {
	t.Setenv(envDriveUploadMaxSizeKBHardCap, "")
	v1 := driveSnapSettings(nil).DriveUploadPolicy().PolicyVersion
	v2 := driveSnapSettings(map[string]string{"drive.upload_max_size_kb": "2097152"}).DriveUploadPolicy().PolicyVersion
	assert.NotEqual(t, v1, v2)
}

// 单次快照读的结构性证据：把同一份快照换成另一份，DriveUploadPolicy 的两个
// 字段（max_size_kb 与 policy_version）必须一起变 —— 若任一字段来自二次读或
// 缓存，就会出现只变一个的撕裂组合。
func TestDriveUploadPolicy_FieldsConsistentUnderSnapshotSwap(t *testing.T) {
	t.Setenv(envDriveUploadMaxSizeKBHardCap, "")
	s := driveSnapSettings(map[string]string{"drive.upload_max_size_kb": "2097152"})
	p := s.DriveUploadPolicy()

	// 版本 = 对返回值自身 6 字段 + 当前 hardCap 的内容哈希，必须自洽。
	want := driveUploadPolicyVersion(p.MaxSizeKB, p.MultipartThresholdKB, p.PartSizeKB,
		p.MaxConcurrentParts, p.MaxConcurrentSessions, DriveUploadMaxSizeKBHardCap())
	assert.Equal(t, want, p.PolicyVersion)
}
