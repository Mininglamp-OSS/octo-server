package group

import (
	"bytes"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/avatarrender"
	"github.com/stretchr/testify/require"
)

func doAvatarGet(t *testing.T, h http.Handler, groupNo, ifNoneMatch string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/groups/"+groupNo+"/avatar", nil)
	require.NoError(t, err)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	h.ServeHTTP(w, req)
	return w
}

// TestGroupAvatarGetAutoNamedRendersIcon 覆盖核心规则（产品 2026-06-29 定稿）：成员名
// 拼接的**自动默认名**（is_named=0）且无自定义文字 → 双人图标（不把拼接名渲成头像文字），
// 即使 name 字段非空。带弱 ETag + must-revalidate；命中 If-None-Match 时 304。
func TestGroupAvatarGetAutoNamedRendersIcon(t *testing.T) {
	s, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	g := New(ctx)

	const groupNo = "avatar_get_autoname_1"
	require.NoError(t, g.db.Insert(&Model{
		GroupNo: groupNo, Name: "张三、李四、王五", Creator: "c1", Status: 1, IsNamed: 0,
	}))

	w := doAvatarGet(t, s.GetRoute(), groupNo, "")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "image/png", w.Header().Get("Content-Type"))
	etag := w.Header().Get("ETag")
	require.True(t, strings.HasPrefix(etag, `W/"`), "weak etag, got %q", etag)
	require.Contains(t, w.Header().Get("Cache-Control"), "must-revalidate")

	img, err := png.Decode(bytes.NewReader(w.Body.Bytes()))
	require.NoError(t, err)
	require.Equal(t, avatarrender.DefaultSize, img.Bounds().Dx())

	// 自动名（is_named=0）→ 双人图标（按 group_no 派生色），不渲染拼接名文字。
	wantIcon, err := avatarrender.RenderIcon(avatarrender.GroupStyleForSeed(groupNo))
	require.NoError(t, err)
	require.Equal(t, wantIcon, w.Body.Bytes(),
		"auto-named group (is_named=0) without custom text must render the two-person icon")

	// 304：带命中的 If-None-Match → 304 无 body。
	w2 := doAvatarGet(t, s.GetRoute(), groupNo, etag)
	require.Equal(t, http.StatusNotModified, w2.Code)
	require.Empty(t, w2.Body.Bytes())
}

// TestGroupAvatarGetNamedRendersNameText 覆盖：用户**显式起名**（is_named=1）且无自定义
// 文字 → 取群名前 2 字（script 感知）渲染文字，而非双人图标。
func TestGroupAvatarGetNamedRendersNameText(t *testing.T) {
	s, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	g := New(ctx)

	const groupNo = "avatar_get_named_text_1"
	require.NoError(t, g.db.Insert(&Model{
		GroupNo: groupNo, Name: "后端架构讨论", Creator: "c1", Status: 1, IsNamed: 1,
	}))

	w := doAvatarGet(t, s.GetRoute(), groupNo, "")
	require.Equal(t, http.StatusOK, w.Code)
	// 命名群也走内容相关弱 ETag：保护改名 / 切自定义触发的缓存失效契约（Jerry-Xin 🔵）。
	etag := w.Header().Get("ETag")
	require.True(t, strings.HasPrefix(etag, `W/"`), "named group avatar must carry a weak ETag, got %q", etag)
	require.Contains(t, w.Header().Get("Cache-Control"), "must-revalidate")

	want, err := avatarrender.RenderGroup(
		avatarrender.GroupNameText("后端架构讨论"),
		avatarrender.GroupStyleForSeed(groupNo),
		avatarrender.DefaultSize,
	)
	require.NoError(t, err)
	require.Equal(t, want, w.Body.Bytes(),
		"named group (is_named=1) without custom text must render script-aware first-2 of the name")

	// 命中的 If-None-Match → 304 无 body（命名群路径同样支持条件请求省渲染）。
	w2 := doAvatarGet(t, s.GetRoute(), groupNo, etag)
	require.Equal(t, http.StatusNotModified, w2.Code)
	require.Empty(t, w2.Body.Bytes())
}

// TestGroupAvatarGetIconFallback 覆盖群名为空 → 同样回退双人图标渲染。
func TestGroupAvatarGetIconFallback(t *testing.T) {
	s, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	g := New(ctx)

	const groupNo = "avatar_get_empty_1"
	require.NoError(t, g.db.Insert(&Model{GroupNo: groupNo, Name: "", Creator: "c1", Status: 1}))

	w := doAvatarGet(t, s.GetRoute(), groupNo, "")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "image/png", w.Header().Get("Content-Type"))

	wantIcon, err := avatarrender.RenderIcon(avatarrender.GroupStyleForSeed(groupNo))
	require.NoError(t, err)
	require.Equal(t, wantIcon, w.Body.Bytes(), "empty name must fall back to group icon")
}

// TestGroupAvatarGetCustomOverrides 覆盖自定义文字+颜色覆盖群名与派生色。
func TestGroupAvatarGetCustomOverrides(t *testing.T) {
	s, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	g := New(ctx)

	const groupNo = "avatar_get_custom_1"
	require.NoError(t, g.db.Insert(&Model{
		GroupNo: groupNo, Name: "原始群名", Creator: "c1", Status: 1,
		AvatarText: "研发", AvatarColor: intPtr(5),
	}))

	w := doAvatarGet(t, s.GetRoute(), groupNo, "")
	require.Equal(t, http.StatusOK, w.Code)

	style, ok := avatarrender.GroupStyleByIndex(5)
	require.True(t, ok)
	want, err := avatarrender.RenderGroup("研发", style, avatarrender.DefaultSize)
	require.NoError(t, err)
	require.Equal(t, want, w.Body.Bytes(), "custom text+color must override name/seed")
}

// TestGroupAvatarGetCustomColorIconNoText 覆盖 S2:自动名群(is_named=0)设了自定义颜色但
// **无**自定义文字 → 渲染该颜色的双人图标(自动名群默认头像与群名无关,但自定义颜色仍被
// 尊重)。fixture 显式 IsNamed=0,名字取「自动名群」以贴合意图(避免与 is_named=1 混淆)。
func TestGroupAvatarGetCustomColorIconNoText(t *testing.T) {
	s, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	g := New(ctx)

	const groupNo = "avatar_get_coloricon_1"
	require.NoError(t, g.db.Insert(&Model{
		GroupNo: groupNo, Name: "自动名群", Creator: "c1", Status: 1, IsNamed: 0, AvatarColor: intPtr(7),
	}))

	w := doAvatarGet(t, s.GetRoute(), groupNo, "")
	require.Equal(t, http.StatusOK, w.Code)

	style, ok := avatarrender.GroupStyleByIndex(7)
	require.True(t, ok)
	wantIcon, err := avatarrender.RenderIcon(style)
	require.NoError(t, err)
	require.Equal(t, wantIcon, w.Body.Bytes(), "auto-named group (is_named=0) with custom color + no text must render the two-person icon in that color")
}

// TestGroupAvatarGetNamedCustomColorRendersNameText 锁定三因子交互(Octo-Q P2-1 建议):
// 命名群(is_named=1) + 自定义颜色 + **无**自定义文字 → 以该自定义颜色渲染群名前 2 字
// (而非派生色、而非图标)。补全 is_named=1 与 custom_color 的组合覆盖。
func TestGroupAvatarGetNamedCustomColorRendersNameText(t *testing.T) {
	s, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	g := New(ctx)

	const groupNo = "avatar_get_named_color_1"
	require.NoError(t, g.db.Insert(&Model{
		GroupNo: groupNo, Name: "后端架构讨论", Creator: "c1", Status: 1, IsNamed: 1, AvatarColor: intPtr(7),
	}))

	w := doAvatarGet(t, s.GetRoute(), groupNo, "")
	require.Equal(t, http.StatusOK, w.Code)

	style, ok := avatarrender.GroupStyleByIndex(7)
	require.True(t, ok)
	want, err := avatarrender.RenderGroup(
		avatarrender.GroupNameText("后端架构讨论"),
		style,
		avatarrender.DefaultSize,
	)
	require.NoError(t, err)
	require.Equal(t, want, w.Body.Bytes(),
		"named group (is_named=1) with custom color + no text must render name first-2 in that custom color")
}

// TestGroupAvatarGetCustomTextNotTruncated 回归 PR#494 评审(Jerry-Xin):用户显式
// 自定义文字必须**原样渲染**(≤4),不得被群名自动取字规则(script 感知前 2)截断 ——
// 4 字自定义渲染全 4 字「研发中心」,而非前 2「研发」。
func TestGroupAvatarGetCustomTextNotTruncated(t *testing.T) {
	s, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	g := New(ctx)

	const groupNo = "avatar_custom_4char_1"
	require.NoError(t, g.db.Insert(&Model{
		GroupNo: groupNo, Name: "原始群名", Creator: "c1", Status: 1,
		AvatarText: "研发中心", // 4 个可见 rune
	}))

	w := doAvatarGet(t, s.GetRoute(), groupNo, "")
	require.Equal(t, http.StatusOK, w.Code)

	style := avatarrender.GroupStyleForSeed(groupNo)
	asIs, err := avatarrender.RenderGroup("研发中心", style, avatarrender.DefaultSize)
	require.NoError(t, err)
	require.Equal(t, asIs, w.Body.Bytes(), "custom avatar_text must render as-is (≤4), not auto-derived")

	truncated, err := avatarrender.RenderGroup("研发", style, avatarrender.DefaultSize)
	require.NoError(t, err)
	require.NotEqual(t, truncated, w.Body.Bytes(), "custom text must NOT be GroupNameText-truncated to 2")
}

// TestGroupAvatarGetUploadedRedirects 覆盖群主已上传自定义头像（is_upload_avatar=1）
// → 维持历史行为：重定向到对象存储，不进服务端渲染。
func TestGroupAvatarGetUploadedRedirects(t *testing.T) {
	s, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	g := New(ctx)

	const groupNo = "avatar_get_uploaded_1"
	const ver int64 = 1733300000000000003
	require.NoError(t, g.db.Insert(&Model{GroupNo: groupNo, Name: "上传群", Creator: "c1", Status: 1}))
	require.NoError(t, g.db.updateAvatar(ctx.GetConfig().GetGroupAvatarFilePath(groupNo, ver), ver, groupNo))

	w := doAvatarGet(t, s.GetRoute(), groupNo, "")
	require.Equal(t, http.StatusFound, w.Code, "uploaded avatar must redirect, not render")
	require.NotEmpty(t, w.Header().Get("Location"))
}

// TestGroupAvatarGetNonexistentReturns404 回归 Fix5:不存在的群不再渲染默认图,返回 404
// (与 UserAvatar 对未知用户一致),消除枚举/无谓渲染面。
func TestGroupAvatarGetNonexistentReturns404(t *testing.T) {
	s, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))

	w := doAvatarGet(t, s.GetRoute(), "no_such_group_xyz", "")
	require.Equal(t, http.StatusNotFound, w.Code, "nonexistent group must 404, not render a default avatar")
	require.Empty(t, w.Body.Bytes())
}

// TestGroupAvatarGetDisbandedReturns404 回归:已解散的群(行仍在但 Status=Disband)同样
// 必须 404,不得在公开端点把其群名渲成 PNG(信息泄露 + 「已解散」vs「从未存在」枚举)。
func TestGroupAvatarGetDisbandedReturns404(t *testing.T) {
	s, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	g := New(ctx)

	const groupNo = "avatar_get_disbanded_1"
	require.NoError(t, g.db.Insert(&Model{
		GroupNo: groupNo, Name: "已解散群", Creator: "c1", Status: GroupStatusDisband,
	}))

	w := doAvatarGet(t, s.GetRoute(), groupNo, "")
	require.Equal(t, http.StatusNotFound, w.Code, "disbanded group must 404, not render its name")
	require.Empty(t, w.Body.Bytes())
}
