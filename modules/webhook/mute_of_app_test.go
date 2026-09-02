package webhook

import "testing"

func TestShouldMuteAppPush_MutedWithWebOnline(t *testing.T) {
	if !shouldMuteAppPush(1, true, false) {
		t.Fatalf("Web 在线且开启静音应抑制 App Push")
	}
}

func TestShouldMuteAppPush_MutedWithPCOnline(t *testing.T) {
	if !shouldMuteAppPush(1, false, true) {
		t.Fatalf("PC 在线且开启静音应抑制 App Push")
	}
}

func TestShouldMuteAppPush_MutedButAllOffline(t *testing.T) {
	if shouldMuteAppPush(1, false, false) {
		t.Fatalf("无从设备在线时不应抑制，保证离线可达")
	}
}

func TestShouldMuteAppPush_NotMuted(t *testing.T) {
	if shouldMuteAppPush(0, true, true) {
		t.Fatalf("未开启静音时绝不应误抑制")
	}
}

func TestFilterMutedAppUIDs_ExcludesMuted(t *testing.T) {
	got := filterMutedAppUIDs([]string{"u1", "u2"}, map[string]struct{}{"u1": {}})
	if len(got) != 1 || got[0] != "u2" {
		t.Fatalf("应剔除 muted uid，期望 [u2]，实际 %v", got)
	}
}

func TestFilterMutedAppUIDs_EmptyMutedKeepsAll(t *testing.T) {
	uids := []string{"u1", "u2"}
	got := filterMutedAppUIDs(uids, nil)
	if len(got) != len(uids) || got[0] != uids[0] || got[1] != uids[1] {
		t.Fatalf("空 muted 集合应返回原列表，实际 %v", got)
	}
	if len(got) > 0 && &got[0] == &uids[0] {
		t.Fatalf("应返回副本而非原切片别名")
	}
}
