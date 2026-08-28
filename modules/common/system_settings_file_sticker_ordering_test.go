package common

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// file.max_size_kb 与 sticker.upload_max_size_kb 的组合不变量（brief D6）。
//
// 全部 no-infra：复用 fileSnapSettings 直接向 snapshot 灌 map。
//
// 背景 —— 为什么这一组用例要单独存在：
// 天花板变成部署可配（OCTO_FILE_MAX_SIZE_KB_HARD_CAP）之前，它是常量
// 524288KB，而贴纸上限最高只到 5120KB，所以 clamp(x, 524288) 永远落不到贴纸
// 上限之下，「全局上限 < 贴纸上限」这个致命组合在读侧不可达，D6 只需要在写侧
// 拦住超管的显式写入。天花板可配之后这条路径打开了：**一次写入都不需要**，
// 空表 + 低天花板开机即违规，而写侧守卫按定义看不见没有发生过的写入。
//
// 所以不变量必须由读侧兜底，写侧守卫只负责「意图不被静默改写」。

// ---------------------------------------------------------------------------
// 读侧：任何配置组合下，生效的贴纸上限都不得高于生效的全局上限
// ---------------------------------------------------------------------------

// 空表 + 低天花板：不需要任何管理台写入就能进入违规态。
// 这是写侧 merge-then-validate 结构上观察不到的那一格。
func TestStickerCapNeverExceedsFileCap_NoWriteNeeded(t *testing.T) {
	t.Setenv(envFileMaxSizeKBHardCap, "512")

	s := fileSnapSettings(map[string]string{}) // system_setting 表里什么都没有

	require.Equal(t, 512, s.FileMaxSizeKB(),
		"前置条件：代码默认 102400 被天花板钳到 512")
	assert.Equal(t, 512, s.StickerUploadMaxSizeKB(),
		"贴纸上限必须收敛到全局上限；否则 1024KB 的贴纸会被前置的全局门拦掉，"+
			"而 appconfig 仍在向客户端广播 1024")
}

func TestStickerCapNeverExceedsFileCap_Matrix(t *testing.T) {
	cases := []struct {
		name       string
		ceiling    string
		snap       map[string]string
		wantFile   int
		wantSticke int
	}{
		{
			name:       "天花板未配置：维持改动前行为",
			ceiling:    "",
			snap:       map[string]string{},
			wantFile:   DefaultFileMaxSizeKB,
			wantSticke: defaultStickerUploadMaxSizeKB,
		},
		{
			name:       "天花板低于贴纸默认值：贴纸被收敛",
			ceiling:    "512",
			snap:       map[string]string{},
			wantFile:   512,
			wantSticke: 512,
		},
		{
			name:       "天花板低于贴纸配置值：贴纸被收敛",
			ceiling:    "2048",
			snap:       map[string]string{"sticker.upload_max_size_kb": "5120"},
			wantFile:   2048,
			wantSticke: 2048,
		},
		{
			name:       "管理台把全局上限压到贴纸之下：贴纸被收敛",
			ceiling:    "",
			snap:       map[string]string{"file.max_size_kb": "800", "sticker.upload_max_size_kb": "5120"},
			wantFile:   800,
			wantSticke: 800,
		},
		{
			name:       "全局上限高于贴纸：贴纸保持自己的值，不被抬高",
			ceiling:    "",
			snap:       map[string]string{"file.max_size_kb": "102400", "sticker.upload_max_size_kb": "2048"},
			wantFile:   102400,
			wantSticke: 2048,
		},
		{
			name:       "直改库写入 ≤0：回落值同样受天花板约束",
			ceiling:    "512",
			snap:       map[string]string{"file.max_size_kb": "-5"},
			wantFile:   512,
			wantSticke: 512,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envFileMaxSizeKBHardCap, tc.ceiling)
			s := fileSnapSettings(tc.snap)

			assert.Equal(t, tc.wantFile, s.FileMaxSizeKB(), "file cap")
			assert.Equal(t, tc.wantSticke, s.StickerUploadMaxSizeKB(), "sticker cap")
			assert.LessOrEqual(t, s.StickerUploadMaxSizeKB(), s.FileMaxSizeKB(),
				"不变量：生效贴纸上限 ≤ 生效全局上限")
		})
	}
}

// appconfig 下发的贴纸上限必须与服务端真正执行的一致 —— 客户端拿它做本地
// 预校验，广播一个服务端不接受的值等于让客户端替用户白排一次队。
func TestAppconfigStickerLimitsNeverAdvertiseAboveFileCap(t *testing.T) {
	t.Setenv(envFileMaxSizeKBHardCap, "512")

	s := fileSnapSettings(map[string]string{"sticker.upload_max_size_kb": "4096"})

	resp := buildStickerUploadLimitsResp(s)
	assert.Equal(t, 512, resp.MaxSizeKB,
		"下发值必须是 min(贴纸上限, 全局上限)")
}

// 管理台 effective_value 同理：显示一个「写着但不生效」的数字，正是本任务在
// extra_allowed_extensions 上明确拒绝的失败形态。
func TestStickerSchemaEffectiveValueReflectsFileCap(t *testing.T) {
	t.Setenv(envFileMaxSizeKBHardCap, "512")

	s := fileSnapSettings(map[string]string{"sticker.upload_max_size_kb": "4096"})

	var def *settingDef
	for i := range systemSettingSchema {
		if systemSettingSchema[i].Category == "sticker" && systemSettingSchema[i].Key == "upload_max_size_kb" {
			def = &systemSettingSchema[i]
			break
		}
	}
	require.NotNil(t, def, "schema 里必须有 sticker.upload_max_size_kb")
	require.NotNil(t, def.Effective)

	assert.Equal(t, "512", def.Effective(s),
		"effective_value 必须是真正生效的值")
}

// ---------------------------------------------------------------------------
// 写侧：守卫比较的两侧都必须是**生效值**
// ---------------------------------------------------------------------------

// 写侧欠拒（under-reject）：cur 来自钳位过的 getter，incoming 却是原样值，
// 守卫因此校验了一对运行时永远不会执行的组合。
func TestApplyFileStickerSizeOverlay_ClampsIncomingAgainstEnvCeiling(t *testing.T) {
	t.Setenv(envFileMaxSizeKBHardCap, "512")

	cur := FileStickerSizeOrdering{FileMaxSizeKB: 512, StickerMaxSizeKB: 1024}
	got := ApplyFileStickerSizeOverlay(cur, map[string]string{"file.max_size_kb": "4096"})

	assert.Equal(t, 512, got.FileMaxSizeKB,
		"prospective 的全局上限必须按天花板钳位后再参与比较")
	assert.True(t, ViolatesFileStickerSizeOrdering(got),
		"钳位后 512 < 1024，这次写入必须被拒 —— 否则超管做一次完全普通的写入，"+
			"拿到 200 {\"applied\":true}，贴纸从此传不上去且没有任何报错")
}

// 另一侧的同一处不对称：贴纸值也要先过它自己的硬上限再比较。
// 这里同时钉死本轮 review 要求明确的决定：**超过贴纸硬上限的写入是钳位接受，
// 不是拒绝** —— 钳位后的 5120 并不与全局上限冲突，没有任何组合是致命的。
func TestApplyFileStickerSizeOverlay_OverCapStickerIsClampedAcceptance(t *testing.T) {
	t.Setenv(envFileMaxSizeKBHardCap, "")

	cur := FileStickerSizeOrdering{FileMaxSizeKB: 6000, StickerMaxSizeKB: 1024}
	got := ApplyFileStickerSizeOverlay(cur, map[string]string{"sticker.upload_max_size_kb": "7000"})

	assert.Equal(t, stickerUploadMaxSizeKBHardCap, got.StickerMaxSizeKB,
		"贴纸写入先被自己的硬上限钳到 5120")
	assert.False(t, ViolatesFileStickerSizeOrdering(got),
		"5120 ≤ 6000，钳位后不冲突：接受")
}

// 反向：钳位后仍然高于全局上限的贴纸写入，必须被拒。
func TestApplyFileStickerSizeOverlay_ClampedStickerStillAboveFileCapIsRejected(t *testing.T) {
	t.Setenv(envFileMaxSizeKBHardCap, "")

	cur := FileStickerSizeOrdering{FileMaxSizeKB: 2000, StickerMaxSizeKB: 1024}
	got := ApplyFileStickerSizeOverlay(cur, map[string]string{"sticker.upload_max_size_kb": "7000"})

	assert.Equal(t, stickerUploadMaxSizeKBHardCap, got.StickerMaxSizeKB)
	assert.True(t, ViolatesFileStickerSizeOrdering(got),
		"5120 > 2000：钳位后依然冲突，拒绝")
}

// 写侧守卫拿到的贴纸值必须是「运营想要的」值，而不是被全局上限收敛过的值 ——
// 否则读侧的 min() 会把两侧拉平，守卫永远不触发。
func TestFileStickerSizeOrdering_UsesUnconvergedStickerValue(t *testing.T) {
	t.Setenv(envFileMaxSizeKBHardCap, "512")

	s := fileSnapSettings(map[string]string{}) // 贴纸默认 1024，全局被钳到 512

	cur := s.FileStickerSizeOrdering()
	assert.Equal(t, 512, cur.FileMaxSizeKB)
	assert.Equal(t, defaultStickerUploadMaxSizeKB, cur.StickerMaxSizeKB,
		"写侧看到的贴纸值必须是 1024（配置意图），不是收敛后的 512；"+
			"用收敛后的值比较等于守卫永远不触发")
	assert.True(t, ViolatesFileStickerSizeOrdering(cur))
}

// ---------------------------------------------------------------------------
// 钳位器本身
// ---------------------------------------------------------------------------

// clampIntUpper 的 ≤0 分支直接 return fallback，从不过上界。天花板是常量
// 524288 时无害（回落值恒低于它）；天花板可配到 100MB 以下之后，一个直改库的
// ≤0 值会拿到高于部署天花板 200 倍的上限。
func TestClampIntUpper_FallbackRespectsHardCap(t *testing.T) {
	s := fileSnapSettings(map[string]string{})

	got := s.clampIntUpper("test.knob", -5, 102400, 512)
	assert.Equal(t, 512, got,
		"回落值也必须过上界，否则天花板形同虚设")

	assert.Equal(t, 100, s.clampIntUpper("test.knob", 0, 100, 512),
		"回落值本就低于上界时保持不变")
}

// 空表时不得把代码默认值当作一次「越界配置」告警：表里什么都没有，
// configured=102400 是在诬告一次没发生过的变更，而且每个 pod 都会打一条。
func TestFileMaxSizeKB_UnconfiguredDoesNotWarnAsConfigured(t *testing.T) {
	t.Setenv(envFileMaxSizeKBHardCap, "512")

	s := fileSnapSettings(map[string]string{})
	require.Equal(t, 512, s.FileMaxSizeKB())

	var warned []string
	s.clampWarned.Range(func(k, _ any) bool {
		warned = append(warned, fmt.Sprint(k))
		return true
	})
	assert.Empty(t, warned,
		"未配置时不应产生越界告警，实际产生了：%v", warned)
}

// 真正配错了当然要告警 —— 上一条不能靠把告警整个删掉来通过。
func TestFileMaxSizeKB_ConfiguredOverCapStillWarns(t *testing.T) {
	t.Setenv(envFileMaxSizeKBHardCap, "512")

	s := fileSnapSettings(map[string]string{"file.max_size_kb": "4096"})
	require.Equal(t, 512, s.FileMaxSizeKB())

	var warned []string
	s.clampWarned.Range(func(k, _ any) bool {
		warned = append(warned, fmt.Sprint(k))
		return true
	})
	assert.NotEmpty(t, warned, "越界的真实配置必须留下一条告警")
}

// 回落分支新增的上界对既有 sticker 键必须是 no-op —— 它们的 hardCap 全是编译期
// 常量且恒高于各自默认值，所以 min(fallback, hardCap) == fallback。
// 真正受影响的只有 file.max_size_kb，因为只有它的天花板是部署可配的。
//
// 这条用例是给未来加键的人看的：若有人加进一个 fallback > hardCap 的键，
// 它的未配置行为会在这里改变，而不是在生产上被发现。
func TestClampIntUpper_FallbackBoundIsNoOpForStickerKnobs(t *testing.T) {
	s := stickerSnapSettings(map[string]string{}) // 全部未配置

	assert.Equal(t, defaultStickerUploadMaxSizeKB, s.stickerUploadMaxSizeKBOwnBound())
	assert.Equal(t, defaultStickerUploadMaxDimension, s.StickerUploadMaxDimension())
	assert.Equal(t, defaultStickerCompressTargetKB, s.StickerCompressTargetKB())
	assert.Equal(t, defaultStickerCompressMaxConcurrency, s.StickerCompressMaxConcurrency())
	assert.Equal(t, defaultStickerCompressTimeoutMs, s.StickerCompressTimeoutMs())
	assert.Equal(t, defaultStickerCompressMaxDimension, s.StickerCompressMaxDimension())
}
