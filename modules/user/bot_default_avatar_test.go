package user

import (
	"bytes"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBotDefaultAvatarAssets(t *testing.T) {
	if len(botDefaultAvatarFiles) != 13 {
		t.Fatalf("botDefaultAvatarFiles len = %d, want 13", len(botDefaultAvatarFiles))
	}
	seen := make(map[string]bool, len(botDefaultAvatarFiles))
	for _, path := range botDefaultAvatarFiles {
		if seen[path] {
			t.Fatalf("duplicate bot default avatar path: %s", path)
		}
		seen[path] = true

		data, err := botDefaultAvatarFS.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if cfg.Width != 512 || cfg.Height != 512 {
			t.Fatalf("%s size = %dx%d, want 512x512", path, cfg.Width, cfg.Height)
		}
	}

	entries, err := botDefaultAvatarFS.ReadDir("assets/bot_default_avatar")
	if err != nil {
		t.Fatalf("read embedded bot default avatar dir: %v", err)
	}
	embeddedCount := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := "assets/bot_default_avatar/" + entry.Name()
		if !seen[path] {
			t.Fatalf("embedded bot default avatar %s is not listed in botDefaultAvatarFiles", path)
		}
		embeddedCount++
	}
	if embeddedCount != len(botDefaultAvatarFiles) {
		t.Fatalf("embedded bot default avatar count = %d, want %d", embeddedCount, len(botDefaultAvatarFiles))
	}
}

func TestReadBotDefaultAvatarDeterministic(t *testing.T) {
	for _, uid := range []string{"27ba6or9nu_bot", "sales_bot_001", "ops_bot_002", "support_bot_003"} {
		t.Run(uid, func(t *testing.T) {
			first, err := readBotDefaultAvatar(uid)
			if err != nil {
				t.Fatalf("first readBotDefaultAvatar: %v", err)
			}
			second, err := readBotDefaultAvatar(uid)
			if err != nil {
				t.Fatalf("second readBotDefaultAvatar: %v", err)
			}
			if !bytes.Equal(first, second) {
				t.Fatal("same bot uid must resolve to the same default avatar bytes")
			}

			idx := botDefaultAvatarIndex(uid)
			if idx < 0 || idx >= len(botDefaultAvatarFiles) {
				t.Fatalf("botDefaultAvatarIndex = %d, want [0,%d)", idx, len(botDefaultAvatarFiles))
			}
		})
	}
}

func TestShouldUseBotDefaultAvatar(t *testing.T) {
	tests := []struct {
		name     string
		uid      string
		userInfo *Model
		want     bool
	}{
		{
			name:     "nil user",
			uid:      "botfather",
			userInfo: nil,
			want:     false,
		},
		{
			name:     "ordinary user",
			uid:      "u_normal",
			userInfo: &Model{UID: "u_normal", Robot: 0},
			want:     false,
		},
		{
			name:     "bot user",
			uid:      "27ba6or9nu_bot",
			userInfo: &Model{UID: "27ba6or9nu_bot", Robot: 1},
			want:     true,
		},
		{
			name:     "system botfather keeps official avatar path",
			uid:      "botfather",
			userInfo: &Model{UID: "botfather", Robot: 1},
			want:     false,
		},
		{
			name:     "notification keeps official avatar path",
			uid:      "notification",
			userInfo: &Model{UID: "notification", Robot: 1},
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldUseBotDefaultAvatar(tt.uid, tt.userInfo)
			if got != tt.want {
				t.Fatalf("shouldUseBotDefaultAvatar(%q) = %v, want %v", tt.uid, got, tt.want)
			}
		})
	}
}

func TestUserAvatarHandlerBotDefaultRouting(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	ctx.GetConfig().Avatar.Default = ""
	ctx.GetConfig().Avatar.DefaultBaseURL = ""

	u := New(ctx)
	for _, m := range []*Model{
		{UID: "avatar_bot_001", Name: "Avatar Bot", Username: "avatar_bot_001", ShortNo: "avbot001", Status: 1, Robot: 1},
		{UID: "avatar_user_001", Name: "Avatar User", Username: "avatar_user_001", ShortNo: "avuser001", Status: 1, Robot: 0},
		{UID: "notification", Name: "Notification", Username: "notification", ShortNo: "notify001", Status: 1, Robot: 1},
	} {
		require.NoError(t, u.db.Insert(m))
	}

	botResp := getAvatarForTest(t, s.GetRoute(), "avatar_bot_001")
	wantBotAvatar, err := readBotDefaultAvatar("avatar_bot_001")
	require.NoError(t, err)
	assert.Equal(t, wantBotAvatar, botResp.Body.Bytes())
	assert.Equal(t, "public, max-age=86400", botResp.Header().Get("Cache-Control"))

	normalResp := getAvatarForTest(t, s.GetRoute(), "avatar_user_001")
	assertGeneratedDefaultAvatarForTest(t, normalResp)
	normalBotAvatar, err := readBotDefaultAvatar("avatar_user_001")
	require.NoError(t, err)
	assert.NotEqual(t, normalBotAvatar, normalResp.Body.Bytes())

	systemResp := getAvatarForTest(t, s.GetRoute(), "notification")
	assertGeneratedDefaultAvatarForTest(t, systemResp)
	systemBotAvatar, err := readBotDefaultAvatar("notification")
	require.NoError(t, err)
	assert.NotEqual(t, systemBotAvatar, systemResp.Body.Bytes())
}

func getAvatarForTest(t *testing.T, handler http.Handler, uid string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/users/"+uid+"/avatar", nil)
	handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "image/png", w.Header().Get("Content-Type"))
	return w
}

func assertGeneratedDefaultAvatarForTest(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	cfg, err := png.DecodeConfig(bytes.NewReader(w.Body.Bytes()))
	require.NoError(t, err)
	assert.Equal(t, 200, cfg.Width)
	assert.Equal(t, 200, cfg.Height)
}
