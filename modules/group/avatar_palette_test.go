package group

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/avatarrender"
	"github.com/stretchr/testify/require"
)

// TestGroupAvatarPalette 覆盖公开色板端点 GET /v1/group/avatar_palette：返回 PaletteSize
// 档、按下标有序、三套色为 #RRGGBB，且与 avatarrender.PaletteHex()（服务端唯一数据源）
// 完全一致——前端据此渲染色圈与本地预览，保证与服务端渲染配色不漂移。无需鉴权。
func TestGroupAvatarPalette(t *testing.T) {
	s, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))

	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/group/avatar_palette", nil)
	require.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Size   int `json:"size"`
		Colors []struct {
			Index    int    `json:"index"`
			Main     string `json:"main"`
			Fill     string `json:"fill"`
			IconBack string `json:"icon_back"`
		} `json:"colors"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	require.Equal(t, avatarrender.PaletteSize(), resp.Size)
	require.Len(t, resp.Colors, avatarrender.PaletteSize())

	hex := avatarrender.PaletteHex()
	for i, cc := range resp.Colors {
		require.Equal(t, i, cc.Index, "colors must be ordered by palette index")
		require.Equal(t, hex[i].Main, cc.Main)
		require.Equal(t, hex[i].Fill, cc.Fill)
		require.Equal(t, hex[i].IconBack, cc.IconBack)
	}
	// 钉死设计稿首档，捕获顺序/取值漂移。
	require.Equal(t, "#14C0FF", resp.Colors[0].Main)

	// 内容相关弱 ETag + 304：带命中的 If-None-Match → 304 无 body（色板变更才会换 ETag）。
	etag := w.Header().Get("ETag")
	require.NotEmpty(t, etag)
	w2 := httptest.NewRecorder()
	req2, err := http.NewRequest("GET", "/v1/group/avatar_palette", nil)
	require.NoError(t, err)
	req2.Header.Set("If-None-Match", etag)
	s.GetRoute().ServeHTTP(w2, req2)
	require.Equal(t, http.StatusNotModified, w2.Code)
	require.Empty(t, w2.Body.Bytes())
}
