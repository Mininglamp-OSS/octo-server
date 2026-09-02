package webhook

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
)

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

func onlineRow(uid string, flag config.DeviceFlag, online int) *config.OnlinestatusResp {
	return &config.OnlinestatusResp{UID: uid, DeviceFlag: flag.Uint8(), Online: online}
}

func TestMutedUIDsFromOnline_WebOnlineMuted(t *testing.T) {
	muted := mutedUIDsFromOnline([]string{"u1"}, []*config.OnlinestatusResp{
		onlineRow("u1", config.Web, 1),
	})
	if _, ok := muted["u1"]; !ok {
		t.Fatalf("Web 在线的候选应被静音，实际 %v", muted)
	}
}

func TestMutedUIDsFromOnline_PCOnlineMuted(t *testing.T) {
	muted := mutedUIDsFromOnline([]string{"u1"}, []*config.OnlinestatusResp{
		onlineRow("u1", config.PC, 1),
	})
	if _, ok := muted["u1"]; !ok {
		t.Fatalf("PC 在线的候选应被静音（防回归到只覆盖 PC 的历史缺口），实际 %v", muted)
	}
}

func TestMutedUIDsFromOnline_NoOnlineRowsNotMuted(t *testing.T) {
	muted := mutedUIDsFromOnline([]string{"u1"}, nil)
	if len(muted) != 0 {
		t.Fatalf("无在线记录时不应静音，保证离线可达，实际 %v", muted)
	}
}

func TestMutedUIDsFromOnline_OfflineRowIgnored(t *testing.T) {
	// online=0 的记录不应被计入在线；APP(主设备) 在线也不触发静音（只看 Web/PC 从设备）
	muted := mutedUIDsFromOnline([]string{"u1"}, []*config.OnlinestatusResp{
		onlineRow("u1", config.Web, 0),
		onlineRow("u1", config.APP, 1),
	})
	if len(muted) != 0 {
		t.Fatalf("离线记录与非 Web/PC 设备不应触发静音，实际 %v", muted)
	}
}

func TestMutedUIDsFromOnline_OnlyCandidatesConsidered(t *testing.T) {
	// u2 虽在线但不在候选集合内，不应出现在结果里
	muted := mutedUIDsFromOnline([]string{"u1"}, []*config.OnlinestatusResp{
		onlineRow("u1", config.Web, 1),
		onlineRow("u2", config.PC, 1),
	})
	if _, ok := muted["u2"]; ok {
		t.Fatalf("非候选 uid 不应被纳入静音集合，实际 %v", muted)
	}
	if len(muted) != 1 {
		t.Fatalf("仅候选 u1 应被静音，实际 %v", muted)
	}
}
