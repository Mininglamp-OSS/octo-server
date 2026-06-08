package opanalytics

import (
	"testing"
	"time"
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

func TestIsExcludedMember(t *testing.T) {
	cases := []struct {
		uid      string
		category string
		want     bool
	}{
		{"u_10000", "", true},
		{"botfather", "", true},
		{"fileHelper", "", true},
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

func TestRollupChannelDaily(t *testing.T) {
	const date = "2026-06-01"
	meta := map[string]*channelMeta{
		"g1":      {spaceID: "s1", convType: convTypeHAGroup, channelType: channelTypeGroup},
		"u_a@u_b": {spaceID: "", convType: convTypeHHPrivate, channelType: channelTypePerson},
	}
	fact3 := []*factMemberChannelDailyModel{
		{StatDate: date, ChannelID: "g1", SenderUID: "alice", SenderType: memberTypeHuman, MsgCount: 3, LastMsgAt: 100},
		{StatDate: date, ChannelID: "g1", SenderUID: "bob", SenderType: memberTypeHuman, MsgCount: 2, LastMsgAt: 200},
		{StatDate: date, ChannelID: "g1", SenderUID: "agentX", SenderType: memberTypeAgent, MsgCount: 5, LastMsgAt: 150},
		{StatDate: date, ChannelID: "u_a@u_b", SenderUID: "u_a", SenderType: memberTypeHuman, MsgCount: 4, LastMsgAt: 90},
	}
	out := rollupChannelDaily(date, fact3, meta)
	byID := map[string]*factChannelDailyModel{}
	for _, r := range out {
		byID[r.ChannelID] = r
	}

	g1 := byID["g1"]
	if g1 == nil {
		t.Fatal("missing g1 rollup")
	}
	if g1.HumanMsgCount != 5 || g1.AgentMsgCount != 5 {
		t.Fatalf("g1 msg: human=%d agent=%d, want 5/5", g1.HumanMsgCount, g1.AgentMsgCount)
	}
	if g1.ActiveHumanMembers != 2 || g1.ActiveAgentMembers != 1 {
		t.Fatalf("g1 active: human=%d agent=%d, want 2/1", g1.ActiveHumanMembers, g1.ActiveAgentMembers)
	}
	if g1.LastMsgAt != 200 {
		t.Fatalf("g1 last_msg_at = %d, want 200", g1.LastMsgAt)
	}
	if g1.SpaceID != "s1" || g1.ConvType != convTypeHAGroup || g1.ChannelType != channelTypeGroup {
		t.Fatalf("g1 meta wrong: space=%q conv=%d type=%d", g1.SpaceID, g1.ConvType, g1.ChannelType)
	}

	dm := byID["u_a@u_b"]
	if dm == nil {
		t.Fatal("missing private rollup")
	}
	if dm.HumanMsgCount != 4 || dm.AgentMsgCount != 0 || dm.ActiveHumanMembers != 1 {
		t.Fatalf("private rollup wrong: %+v", dm)
	}
	if dm.SpaceID != "" || dm.ChannelType != channelTypePerson {
		t.Fatalf("private meta wrong: space=%q type=%d", dm.SpaceID, dm.ChannelType)
	}
}
