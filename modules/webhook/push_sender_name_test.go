package webhook

import "testing"

// TestFallbackSyntheticSenderName_KeepsExistingName 验证：当 GetThirdName 已取到常用名
// （真实用户）时，兜底逻辑直接原样返回、cacheable=true，也不触碰 datasource（因此传 nil
// ctx 也安全）。只有名字为空（如 incoming webhook 虚拟发送者）才会走解析兜底。
func TestFallbackSyntheticSenderName_KeepsExistingName(t *testing.T) {
	got, cacheable := fallbackSyntheticSenderName(nil, "iwh_abc", "Alice")
	if got != "Alice" || !cacheable {
		t.Fatalf("fallbackSyntheticSenderName(nil, iwh_abc, Alice) = (%q, %v), want (Alice, true)", got, cacheable)
	}
}

// TestRepairEmptyCachedName_KeepsExistingName 验证缓存命中且 name 非空时,修复逻辑原样
// 返回、不触碰 datasource/Redis(因此 nil ctx 也安全)。只有命中到空名(存量 iwh_)才会
// 走兜底解析并就地修复。
func TestRepairEmptyCachedName_KeepsExistingName(t *testing.T) {
	if got := repairEmptyCachedName(nil, "name:k", "iwh_abc", "Alice"); got != "Alice" {
		t.Fatalf("repairEmptyCachedName overrode a present cached name: got %q, want %q", got, "Alice")
	}
}
