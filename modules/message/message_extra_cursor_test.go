package message

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSyncMessageExtraRejectsEmptyChannelBeforeStorageAccess(t *testing.T) {
	// A nil-backed Message is deliberate: if validation ever moves behind the DB
	// or Redis path again, this test panics instead of silently allowing storage
	// access for an invalid channel.
	m := &Message{}
	r := helperHarness(m.syncMessageExtra)
	req := httptest.NewRequest(http.MethodGet, "/probe", strings.NewReader(`{"channel_id":" ","extra_version":1152921504606846976}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want %d; body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

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
			name:       "negative issued max normalizes to empty range",
			requested:  20,
			cached:     0,
			issuedMax:  -1,
			wantCursor: 0,
			wantStore:  false,
		},
		{
			name:       "negative cache heals from valid request",
			requested:  20,
			cached:     -5,
			issuedMax:  100,
			wantCursor: 20,
			wantStore:  true,
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
			upperBound := tt.issuedMax
			if upperBound < 0 {
				upperBound = 0
			}
			if gotCursor < 0 || gotCursor > upperBound {
				t.Fatalf("resolved cursor %d escaped issued range [0,%d]", gotCursor, upperBound)
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

func TestMessageExtraPoisonedCursorSelfHealsInRedis(t *testing.T) {
	m, ctx := newSeqTestMessage(t, 0, 0)
	const (
		messageID   = "cursor-heal-message"
		channelID   = "cursor-heal-channel"
		channelType = uint8(2)
		uid         = "cursor-heal-user"
		source      = "-cursor-heal-source"
		requested   = int64(20)
		issued      = int64(100)
		poisoned    = int64(1 << 60)
	)
	redisKey := "messageExtraVersion:" + uid + source

	if _, err := ctx.DB().UpdateBySql("DELETE FROM `message_extra` WHERE `message_id`=?", messageID).Exec(); err != nil {
		t.Fatalf("clean cursor fixture: %v", err)
	}
	if _, err := ctx.DB().UpdateBySql(
		"INSERT INTO `message_extra` (`message_id`,`channel_id`,`channel_type`,`version`) VALUES (?,?,?,?)",
		messageID, channelID, channelType, issued,
	).Exec(); err != nil {
		t.Fatalf("seed cursor fixture: %v", err)
	}
	redis := ctx.GetRedisConn()
	if err := m.setMessageExtraVersion(uid, channelID, channelType, source, poisoned); err != nil {
		t.Fatalf("seed poisoned Redis cursor: %v", err)
	}
	t.Cleanup(func() {
		_, _ = ctx.DB().UpdateBySql("DELETE FROM `message_extra` WHERE `message_id`=?", messageID).Exec()
		_ = redis.Del(redisKey)
	})

	cached, err := m.getMessageExtraVersion(uid, source, channelID, channelType)
	if err != nil {
		t.Fatalf("get poisoned cursor: %v", err)
	}
	if cached != poisoned {
		t.Fatalf("cached cursor=%d want poisoned value %d", cached, poisoned)
	}
	issuedMax, err := m.messageExtraDB.maxVersion(channelID, channelType)
	if err != nil {
		t.Fatalf("maxVersion: %v", err)
	}
	normalized, persist := resolveMessageExtraSyncCursor(requested, cached, issuedMax)
	if normalized != requested || !persist {
		t.Fatalf("resolved cursor=(%d,%v), want (%d,true)", normalized, persist, requested)
	}
	if err := m.setMessageExtraVersion(uid, channelID, channelType, source, normalized); err != nil {
		t.Fatalf("heal Redis cursor: %v", err)
	}
	healed, err := m.getMessageExtraVersion(uid, source, channelID, channelType)
	if err != nil {
		t.Fatalf("get healed cursor: %v", err)
	}
	if healed != requested {
		t.Fatalf("healed cursor=%d want %d", healed, requested)
	}
}
