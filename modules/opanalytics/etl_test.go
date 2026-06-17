package opanalytics

import (
	"testing"
	"time"

	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
)

func TestDayWindowUnix(t *testing.T) {
	loc := reportLocation()
	exp, err := time.ParseInLocation("2006-01-02", "2026-06-01", loc)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	start, end, err := dayWindowUnix("2026-06-01")
	if err != nil {
		t.Fatalf("dayWindowUnix: %v", err)
	}
	if start != exp.Unix() {
		t.Fatalf("start = %d, want %d", start, exp.Unix())
	}
	if end != exp.AddDate(0, 0, 1).Unix() {
		t.Fatalf("end = %d, want %d", end, exp.AddDate(0, 0, 1).Unix())
	}
	if end-start != 24*3600 {
		t.Fatalf("window = %d sec, want 86400", end-start)
	}

	// 边界：当日 23:30 落在 [start,end)，次日 00:00 不落在本窗口。
	lastMoment := exp.Add(23*time.Hour + 30*time.Minute).Unix()
	if !(lastMoment >= start && lastMoment < end) {
		t.Fatalf("23:30 ts %d not within [%d,%d)", lastMoment, start, end)
	}
	nextMidnight := exp.AddDate(0, 0, 1).Unix()
	if nextMidnight < end {
		t.Fatalf("next midnight %d should be >= end %d", nextMidnight, end)
	}
}

func TestNormalizePrivatePair(t *testing.T) {
	cases := []struct {
		in       string
		wantA    string
		wantB    string
		wantOK   bool
		scenario string
	}{
		{"u_b@u_a", "u_a", "u_b", true, "hash-order normalized to lexical"},
		{"u_a@u_b", "u_a", "u_b", true, "already lexical"},
		{"u_a@u_a", "u_a", "u_a", true, "same (degenerate but parseable)"},
		{"x@y@z", "", "", false, "uid contains @ -> 3 parts"},
		{"u_a@", "", "", false, "empty second"},
		{"@u_b", "", "", false, "empty first"},
		{"noat", "", "", false, "no @"},
	}
	for _, c := range cases {
		a, b, ok := normalizePrivatePair(c.in)
		if ok != c.wantOK || a != c.wantA || b != c.wantB {
			t.Fatalf("%s: normalizePrivatePair(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.scenario, c.in, a, b, ok, c.wantA, c.wantB, c.wantOK)
		}
	}
}

// TestNormalizePrivatePairStripsSpacePrefix 覆盖 issue #392 语义修复：私聊 channel_id
// 两端带 Space/适配器前缀(s{32hex}_uid / sminglue_default_uid)时，须反解为裸 uid 再规范化，
// 以对齐 dim_member.uid。hex 空间走正则回退无需注册；命名空间需注册才能反解。
func TestNormalizePrivatePairStripsSpacePrefix(t *testing.T) {
	const hex32 = "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6"
	spacepkg.RegisterSpaceIDs([]string{"minglue_default"})
	defer spacepkg.RegisterSpaceIDs(nil)

	cases := []struct {
		in           string
		wantA, wantB string
		wantOK       bool
		scenario     string
	}{
		{"s" + hex32 + "_u_alice@s" + hex32 + "_u_bob", "u_alice", "u_bob", true, "both space-prefixed (regex fallback)"},
		{"s" + hex32 + "_u_bob@s" + hex32 + "_u_alice", "u_alice", "u_bob", true, "prefixed + hash-order swapped -> lexical"},
		{"u_alice@s" + hex32 + "_u_bob", "u_alice", "u_bob", true, "mixed bare + prefixed"},
		{"sminglue_default_botfather@u_alice", "botfather", "u_alice", true, "bot-adapter prefix via registered named space"},
		{"u_alice@u_bob", "u_alice", "u_bob", true, "both bare (unchanged)"},
	}
	for _, c := range cases {
		a, b, ok := normalizePrivatePair(c.in)
		if ok != c.wantOK || a != c.wantA || b != c.wantB {
			t.Fatalf("%s: normalizePrivatePair(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.scenario, c.in, a, b, ok, c.wantA, c.wantB, c.wantOK)
		}
	}
}

func TestIsExcludedMember(t *testing.T) {
	cases := []struct {
		uid      string
		category string
		want     bool
	}{
		{"u_10000", "", true},      // pkg/space.SystemBots
		{"botfather", "", true},    // pkg/space.SystemBots
		{"fileHelper", "", true},   // pkg/space.SystemBots
		{"notification", "", true}, // pkg/space.SystemBots(此前硬编码名单漏掉)
		{"someone", "system", true},
		{"someone", "normal", false},
		{"alice", "", false},
	}
	for _, c := range cases {
		if got := isExcludedMember(c.uid, c.category); got != c.want {
			t.Fatalf("isExcludedMember(%q,%q) = %v, want %v", c.uid, c.category, got, c.want)
		}
	}
}

func TestConvType(t *testing.T) {
	if groupConvType(0) != convTypeHHGroup {
		t.Fatalf("group no-agent should be HH群")
	}
	if groupConvType(2) != convTypeHAGroup {
		t.Fatalf("group with agent should be HA群")
	}
	if privateConvType(memberTypeHuman, memberTypeHuman) != convTypeHHPrivate {
		t.Fatalf("human-human should be HH私聊")
	}
	if privateConvType(memberTypeHuman, memberTypeAgent) != convTypeHAPrivate {
		t.Fatalf("human-agent should be HA私聊")
	}
	if privateConvType(memberTypeAgent, memberTypeHuman) != convTypeHAPrivate {
		t.Fatalf("agent-human should be HA私聊")
	}
}

// TestAggregateChunk 校验 chunk 聚合的纯逻辑：排除系统/测试账号、私聊任一方被排除则丢弃、
// 群消息按 human/agent 归类、脏日去重。
func TestAggregateChunk(t *testing.T) {
	const ts = int64(1_780_000_000) // 落在某报告日内
	day := time.Unix(ts, 0).In(reportLocation()).Format("2006-01-02")

	memberType := map[string]uint8{
		"alice": memberTypeHuman, "bob": memberTypeHuman, "agentX": memberTypeAgent,
		"botfather": memberTypeAgent, "u_test": memberTypeHuman,
	}
	excluded := map[string]bool{"u_test": true} // category=system
	excludedUID := func(uid string) bool {
		// 复刻 RunIncremental 里的谓词：系统 bot ∪ excluded 集
		return uid == "botfather" || uid == "u_10000" || uid == "fileHelper" || uid == "notification" || excluded[uid]
	}
	groupMeta := map[string]groupMetaInfo{
		"g1": {spaceID: "s1", convType: convTypeHAGroup},
	}

	rows := []*srcMessageRow{
		{ID: 1, FromUID: "alice", ChannelID: "g1", ChannelType: channelTypeGroup, Timestamp: ts},
		{ID: 2, FromUID: "bob", ChannelID: "g1", ChannelType: channelTypeGroup, Timestamp: ts + 1},
		{ID: 3, FromUID: "agentX", ChannelID: "g1", ChannelType: channelTypeGroup, Timestamp: ts + 2},
		{ID: 4, FromUID: "botfather", ChannelID: "g1", ChannelType: channelTypeGroup, Timestamp: ts + 3},       // 系统bot→剔除
		{ID: 5, FromUID: "u_test", ChannelID: "g1", ChannelType: channelTypeGroup, Timestamp: ts + 4},          // 测试账号→剔除
		{ID: 6, FromUID: "alice", ChannelID: "alice@bob", ChannelType: channelTypePerson, Timestamp: ts},       // HH私聊
		{ID: 7, FromUID: "alice", ChannelID: "alice@botfather", ChannelType: channelTypePerson, Timestamp: ts}, // 一方系统bot→整条丢弃
	}

	res := aggregateChunk(rows, memberType, excludedUID, groupMeta)

	// ③：g1 仅 alice/bob/agentX，私聊仅 alice@bob 的 alice
	got := map[string]int{}
	for _, f := range res.fact3 {
		got[f.ChannelID+"/"+f.SenderUID] = f.MsgCount
	}
	want := map[string]int{"g1/alice": 1, "g1/bob": 1, "g1/agentX": 1, "alice@bob/alice": 1}
	if len(got) != len(want) {
		t.Fatalf("fact3 keys = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("fact3[%s] = %d, want %d (full=%v)", k, got[k], v, got)
		}
	}
	if _, ok := got["g1/botfather"]; ok {
		t.Fatal("system bot must be excluded from fact3")
	}
	if _, ok := got["g1/u_test"]; ok {
		t.Fatal("category=system account must be excluded from fact3")
	}
	for _, f := range res.fact3 {
		if f.ChannelID == "alice@botfather" {
			t.Fatal("private chat with system bot must be dropped entirely")
		}
	}

	// 脏日去重：只有一个 day
	if len(res.dirtyDays) != 1 || res.dirtyDays[0] != day {
		t.Fatalf("dirtyDays = %v, want [%s]", res.dirtyDays, day)
	}
}
