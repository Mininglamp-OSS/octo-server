package avatarrender

import (
	"os"
	"path/filepath"
	"testing"
)

// TestGenerateGroupSamples 仅在设置 AVATAR_GROUP_SAMPLE_DIR 时运行，写出群默认头像
// 样张供肉眼比对设计稿（文字模式 + 图标兜底）。例：
//
//	AVATAR_GROUP_SAMPLE_DIR=.context/group-samples go test ./pkg/avatarrender/ -run TestGenerateGroupSamples -v
func TestGenerateGroupSamples(t *testing.T) {
	dir := os.Getenv("AVATAR_GROUP_SAMPLE_DIR")
	if dir == "" {
		t.Skip("set AVATAR_GROUP_SAMPLE_DIR to generate group sample PNGs")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// (group_no, 群名) — group_no 决定派生色，群名按 GroupNameText 自动取字
	// (script 感知前 2：汉字前2 / 纯数字前2 / 纯英文首字母缩写 / 否则空→图标)。
	samples := []struct{ groupNo, name string }{
		{"g01", "后端架构讨论"},      // 含汉字 → 前 2「后端」
		{"g02", "Bug反馈群"},      // 混排 → 只取汉字「反馈」
		{"g03", "2024春招群"},     // 数字+汉字 → 「春招」
		{"g04", "Backend Team"}, // 纯英文 → 首字母缩写「BT」
		{"g05", "Sales"},        // 单个单词 → 「S」
		{"g06", "2024"},         // 纯数字 → 前 2「20」
		{"g07", "🎉🎉"},           // emoji 无法成字 → 图标兜底
		{"g08", ""},             // 空名 → 图标兜底
	}
	for _, s := range samples {
		text := GroupNameText(s.name)
		if text == "" {
			// 空/纯符号/emoji → 回退群组双人图标。
			data, err := RenderIcon(GroupStyleForSeed(s.groupNo))
			if err != nil {
				t.Fatalf("render icon %s: %v", s.groupNo, err)
			}
			if err := os.WriteFile(filepath.Join(dir, s.groupNo+"_icon.png"), data, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		data, err := RenderGroup(text, GroupStyleForSeed(s.groupNo), 200)
		if err != nil {
			t.Fatalf("render %s: %v", s.name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, s.groupNo+"_"+text+".png"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("wrote group samples to %s", dir)
}
