package cardmsg

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestAcceptedProfilesSingleAuthority 守卫 D12 单一权威（brief D12.2）：能力清单下发的
// profiles（AcceptedProfiles）与校验器接受集（interactiveByProfile）必须恒一致 —— 二者
// 现同源于 acceptedProfiles，但本测试防止将来重新引入硬编码 switch 造成漂移。
func TestAcceptedProfilesSingleAuthority(t *testing.T) {
	got := AcceptedProfiles()

	// ① 对外清单 == 校验器接受集：每个 advertised profile 都被 interactiveByProfile 接受。
	for _, p := range got {
		_, ok := interactiveByProfile(p)
		assert.Truef(t, ok, "advertised profile %q must be accepted by the validator", p)
	}

	// ② 精确集合与顺序（v1→v2）——additive-only 演进：将来新增 octo/v3 会在此显式变更。
	assert.Equal(t, []string{ProfileV1, ProfileV2}, got)

	// ③ 交互档位与校验器一致：v1 展示档(false)、v2 交互档(true)。
	if in, ok := interactiveByProfile(ProfileV1); assert.True(t, ok) {
		assert.False(t, in, "octo/v1 是展示档")
	}
	if in, ok := interactiveByProfile(ProfileV2); assert.True(t, ok) {
		assert.True(t, in, "octo/v2 是交互档")
	}

	// ④ 未知 profile 一律不接受（fail-closed）。
	for _, bad := range []string{"", "octo/v0", "octo/v3", "octo/v2 ", "OCTO/V2", "v2"} {
		_, ok := interactiveByProfile(bad)
		assert.Falsef(t, ok, "unknown profile %q must not be accepted", bad)
	}
}

// TestAcceptedProfilesReturnsFreshSlice 确认返回值是拷贝：调用方改动不得污染内部权威。
func TestAcceptedProfilesReturnsFreshSlice(t *testing.T) {
	a := AcceptedProfiles()
	assert.NotEmpty(t, a)
	a[0] = "tampered"
	b := AcceptedProfiles()
	assert.Equal(t, ProfileV1, b[0], "内部 acceptedProfiles 不得被调用方改动影响")
}
