package message

import "testing"

func TestParseCachedMessageExtraSyncCursorMarksMalformedValueInvalid(t *testing.T) {
	tests := []struct {
		raw  string
		want int64
	}{
		{raw: "", want: 0},
		{raw: "42", want: 42},
		{raw: "not-an-integer", want: -1},
	}
	for _, tt := range tests {
		if got := parseCachedMessageExtraSyncCursor(tt.raw); got != tt.want {
			t.Fatalf("parseCachedMessageExtraSyncCursor(%q)=%d want %d", tt.raw, got, tt.want)
		}
	}
}

func TestResolveMessageExtraSyncCursor(t *testing.T) {
	tests := []struct {
		name       string
		requested  int64
		cached     int64
		issuedMax  int64
		wantCursor int64
		wantStore  bool
	}{
		{
			name:       "valid request advances cache",
			requested:  80,
			cached:     50,
			issuedMax:  100,
			wantCursor: 80,
			wantStore:  true,
		},
		{
			name:       "valid cache prevents rollback",
			requested:  50,
			cached:     80,
			issuedMax:  100,
			wantCursor: 80,
			wantStore:  false,
		},
		{
			name:       "oversized request clamps to issued max",
			requested:  1 << 60,
			cached:     50,
			issuedMax:  100,
			wantCursor: 100,
			wantStore:  true,
		},
		{
			name:       "negative request clamps to zero",
			requested:  -1,
			cached:     0,
			issuedMax:  100,
			wantCursor: 0,
			wantStore:  false,
		},
		{
			name:       "poisoned cache heals from valid request",
			requested:  20,
			cached:     1 << 60,
			issuedMax:  100,
			wantCursor: 20,
			wantStore:  true,
		},
		{
			name:       "poisoned cache and request heal to issued max",
			requested:  1 << 60,
			cached:     1 << 60,
			issuedMax:  100,
			wantCursor: 100,
			wantStore:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotCursor, gotStore := resolveMessageExtraSyncCursor(tt.requested, tt.cached, tt.issuedMax)
			if gotCursor != tt.wantCursor || gotStore != tt.wantStore {
				t.Fatalf("resolveMessageExtraSyncCursor(%d,%d,%d)=(%d,%v), want (%d,%v)",
					tt.requested, tt.cached, tt.issuedMax, gotCursor, gotStore, tt.wantCursor, tt.wantStore)
			}
			if gotCursor < 0 || gotCursor > tt.issuedMax {
				t.Fatalf("resolved cursor %d escaped issued range [0,%d]", gotCursor, tt.issuedMax)
			}
		})
	}
}

func TestMessageExtraMaxVersionIsChannelScoped(t *testing.T) {
	m, ctx := newSeqTestMessage(t, 0, 0)
	const prefix = "cursor-max-test-"
	if _, err := ctx.DB().UpdateBySql("DELETE FROM `message_extra` WHERE `message_id` LIKE ?", prefix+"%").Exec(); err != nil {
		t.Fatalf("clean cursor fixtures: %v", err)
	}
	t.Cleanup(func() {
		_, _ = ctx.DB().UpdateBySql("DELETE FROM `message_extra` WHERE `message_id` LIKE ?", prefix+"%").Exec()
	})

	fixtures := []struct {
		messageID   string
		channelID   string
		channelType uint8
		version     int64
	}{
		{prefix + "1", "cursor-channel", 2, 10},
		{prefix + "2", "cursor-channel", 2, 20},
		{prefix + "3", "other-channel", 2, 999},
		{prefix + "4", "cursor-channel", 1, 888},
	}
	for _, fixture := range fixtures {
		if _, err := ctx.DB().UpdateBySql(
			"INSERT INTO `message_extra` (`message_id`,`channel_id`,`channel_type`,`version`) VALUES (?,?,?,?)",
			fixture.messageID, fixture.channelID, fixture.channelType, fixture.version,
		).Exec(); err != nil {
			t.Fatalf("seed cursor fixture: %v", err)
		}
	}

	got, err := m.messageExtraDB.maxVersion("cursor-channel", 2)
	if err != nil {
		t.Fatalf("maxVersion: %v", err)
	}
	if got != 20 {
		t.Fatalf("maxVersion=%d want 20", got)
	}
	empty, err := m.messageExtraDB.maxVersion("empty-channel", 2)
	if err != nil {
		t.Fatalf("maxVersion empty: %v", err)
	}
	if empty != 0 {
		t.Fatalf("empty maxVersion=%d want 0", empty)
	}
}
