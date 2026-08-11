package group

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/require"
)

// putGroupUpdate 以 group 创建者身份 PUT /v1/groups/:group_no 改群信息。
func putGroupUpdate(t *testing.T, h http.Handler, groupNo string, body map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, err := http.NewRequest("PUT", "/v1/groups/"+groupNo, bytes.NewReader([]byte(util.ToJson(body))))
	require.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	h.ServeHTTP(w, req)
	return w
}

// seedCreatorGroup 建一个由 testutil.UID（=token 对应用户）创建的群，并写入创建者
// 成员行，使其能通过 groupUpdate 的管理员校验。
func seedCreatorGroup(t *testing.T, g *Group, groupNo, name string) {
	t.Helper()
	require.NoError(t, g.db.Insert(&Model{GroupNo: groupNo, Name: name, Creator: testutil.UID, Status: 1}))
	require.NoError(t, g.db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: testutil.UID, Role: MemberRoleCreator, Status: 1,
	}))
}

// TestGroupUpdateAvatarCustom 覆盖改群接口设置/清除自定义头像文字与颜色的落库。
func TestGroupUpdateAvatarCustom(t *testing.T) {
	s, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	g := New(ctx)

	const groupNo = "avatar_upd_1"
	seedCreatorGroup(t, g, groupNo, "原始群名")

	// 设置自定义文字 + 颜色。
	w := putGroupUpdate(t, s.GetRoute(), groupNo, map[string]string{
		attrKeyAvatarText:  "研发",
		attrKeyAvatarColor: "5",
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	got, err := g.db.QueryWithGroupNo(groupNo)
	require.NoError(t, err)
	require.Equal(t, "研发", got.AvatarText)
	require.NotNil(t, got.AvatarColor)
	require.Equal(t, 5, *got.AvatarColor)

	// 只改颜色，文字保持现值。
	w = putGroupUpdate(t, s.GetRoute(), groupNo, map[string]string{attrKeyAvatarColor: "8"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	got, err = g.db.QueryWithGroupNo(groupNo)
	require.NoError(t, err)
	require.Equal(t, "研发", got.AvatarText, "未提供 avatar_text 时文字应保持")
	require.NotNil(t, got.AvatarColor)
	require.Equal(t, 8, *got.AvatarColor)

	// 清除自定义（文字空串、颜色 -1）→ 回退默认派生。
	w = putGroupUpdate(t, s.GetRoute(), groupNo, map[string]string{
		attrKeyAvatarText:  "",
		attrKeyAvatarColor: "-1",
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	got, err = g.db.QueryWithGroupNo(groupNo)
	require.NoError(t, err)
	require.Equal(t, "", got.AvatarText)
	require.Nil(t, got.AvatarColor, "清除后颜色应为 NULL")
}

// TestGroupUpdateAvatarCustomValidation 覆盖改群接口对自定义头像参数的校验。
func TestGroupUpdateAvatarCustomValidation(t *testing.T) {
	s, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	g := New(ctx)

	const groupNo = "avatar_upd_val_1"
	seedCreatorGroup(t, g, groupNo, "校验群")

	cases := []struct {
		name string
		body map[string]string
	}{
		{"text over 4 visible runes", map[string]string{attrKeyAvatarText: "一二三四五"}},
		{"color out of range", map[string]string{attrKeyAvatarColor: "99"}},
		{"color non-numeric", map[string]string{attrKeyAvatarColor: "abc"}},
		{"clear uploaded invalid", map[string]string{attrKeyAvatarText: "研发", attrKeyClearUploadedAvatar: "yes"}},
		{"clear uploaded alone", map[string]string{attrKeyClearUploadedAvatar: "1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := putGroupUpdate(t, s.GetRoute(), groupNo, tc.body)
			// D14 兼容：错误响应统一 wire 400。
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		})
	}

	// 非法请求不得落库。
	got, err := g.db.QueryWithGroupNo(groupNo)
	require.NoError(t, err)
	require.Equal(t, "", got.AvatarText)
	require.Nil(t, got.AvatarColor)
}

// TestGroupUpdateAvatarCustomClearsUploadedAvatar 覆盖从上传图片头像切回服务端生成
// 文字/颜色头像的原子切换语义：同一次 PUT 写 avatar_text/avatar_color 并清除上传图优先级。
func TestGroupUpdateAvatarCustomClearsUploadedAvatar(t *testing.T) {
	s, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	g := New(ctx)

	const groupNo = "avatar_upd_clear_upload_1"
	oldColor := 2
	require.NoError(t, g.db.Insert(&Model{
		GroupNo: groupNo, Name: "上传头像群", Creator: testutil.UID, Status: 1,
		Version: 100, IsUploadAvatar: 1, AvatarText: "旧字", AvatarColor: &oldColor,
	}))
	require.NoError(t, g.db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: testutil.UID, Role: MemberRoleCreator, Status: 1,
	}))

	w := putGroupUpdate(t, s.GetRoute(), groupNo, map[string]string{
		attrKeyAvatarText:          "研发",
		attrKeyAvatarColor:         "5",
		attrKeyClearUploadedAvatar: "1",
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	got, err := g.db.QueryWithGroupNo(groupNo)
	require.NoError(t, err)
	require.Equal(t, "研发", got.AvatarText)
	require.NotNil(t, got.AvatarColor)
	require.Equal(t, 5, *got.AvatarColor)
	require.Equal(t, 0, got.IsUploadAvatar, "clear_uploaded_avatar=1 应清除上传图优先级")
	require.NotEqual(t, int64(100), got.Version, "切换头像来源应 bump version")

	// 只改文字 + clear：颜色保持；clear_uploaded_avatar 也接受 true。
	require.NoError(t, g.db.updateAvatar("uploaded/path-1.png", 1, groupNo))
	w = putGroupUpdate(t, s.GetRoute(), groupNo, map[string]string{
		attrKeyAvatarText:          "产品",
		attrKeyClearUploadedAvatar: "true",
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	got, err = g.db.QueryWithGroupNo(groupNo)
	require.NoError(t, err)
	require.Equal(t, "产品", got.AvatarText)
	require.NotNil(t, got.AvatarColor)
	require.Equal(t, 5, *got.AvatarColor, "只改文字 + clear 时颜色保持")
	require.Equal(t, 0, got.IsUploadAvatar)

	// 只改颜色 + clear：文字保持。
	require.NoError(t, g.db.updateAvatar("uploaded/path-2.png", 2, groupNo))
	w = putGroupUpdate(t, s.GetRoute(), groupNo, map[string]string{
		attrKeyAvatarColor:         "7",
		attrKeyClearUploadedAvatar: "1",
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	got, err = g.db.QueryWithGroupNo(groupNo)
	require.NoError(t, err)
	require.Equal(t, "产品", got.AvatarText, "只改颜色 + clear 时文字保持")
	require.NotNil(t, got.AvatarColor)
	require.Equal(t, 7, *got.AvatarColor)
	require.Equal(t, 0, got.IsUploadAvatar)
}

// TestGroupUpdatePartialWriteRejected 回归:混合 payload(合法 name + 非法 avatar)
// 必须整体拒绝(400)且**不**部分写入群名——校验已前置到任何 mutation 之前。
func TestGroupUpdatePartialWriteRejected(t *testing.T) {
	s, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	g := New(ctx)

	const groupNo = "avatar_upd_partial_1"
	const origName = "原始群名"
	seedCreatorGroup(t, g, groupNo, origName)

	cases := []struct {
		name string
		body map[string]string
	}{
		{"valid name + non-numeric color", map[string]string{"name": "新名字", attrKeyAvatarColor: "abc"}},
		{"valid name + out-of-range color", map[string]string{"name": "新名字", attrKeyAvatarColor: "99"}},
		{"valid name + over-long text", map[string]string{"name": "新名字", attrKeyAvatarText: "一二三四五"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := putGroupUpdate(t, s.GetRoute(), groupNo, tc.body)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

			got, err := g.db.QueryWithGroupNo(groupNo)
			require.NoError(t, err)
			require.Equal(t, origName, got.Name, "群名不得在 avatar 校验失败时被部分写入")
		})
	}
}

// TestGroupUpdateAvatarColumnScoped 回归 Fix2:只更新本次提供的列——只改文字不动颜色、
// 只改颜色不动文字(列级 UPDATE,无读-改-写竞态)。并覆盖 avatar_color 空串清除路径。
func TestGroupUpdateAvatarColumnScoped(t *testing.T) {
	s, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	g := New(ctx)

	const groupNo = "avatar_upd_scoped_1"
	seedCreatorGroup(t, g, groupNo, "范围群")

	// 先设文字+色。
	require.Equal(t, http.StatusOK, putGroupUpdate(t, s.GetRoute(), groupNo, map[string]string{
		attrKeyAvatarText: "研发", attrKeyAvatarColor: "5",
	}).Code)

	// 只改文字 → 颜色保持 5。
	require.Equal(t, http.StatusOK, putGroupUpdate(t, s.GetRoute(), groupNo, map[string]string{
		attrKeyAvatarText: "产品",
	}).Code)
	got, err := g.db.QueryWithGroupNo(groupNo)
	require.NoError(t, err)
	require.Equal(t, "产品", got.AvatarText)
	require.NotNil(t, got.AvatarColor)
	require.Equal(t, 5, *got.AvatarColor, "只改文字时颜色列不得被动")

	// avatar_color 空串 → 清除颜色，文字保持。
	require.Equal(t, http.StatusOK, putGroupUpdate(t, s.GetRoute(), groupNo, map[string]string{
		attrKeyAvatarColor: "",
	}).Code)
	got, err = g.db.QueryWithGroupNo(groupNo)
	require.NoError(t, err)
	require.Equal(t, "产品", got.AvatarText, "只改颜色时文字列不得被动")
	require.Nil(t, got.AvatarColor, "空串应清除自定义色")
}

// TestGroupUpdateAvatarCustomSkipsDisbanded 回归 disband TOCTOU 关闭:updateAvatarCustom
// 的 WHERE 带 status<>disband——对已解散的群命中 0 行,不写 avatar 列、不 bump version。
// 这正是服务层「读到未解散」之后、写入之前群被并发解散时的兜底:0 行 → not-found/disbanded。
func TestGroupUpdateAvatarCustomSkipsDisbanded(t *testing.T) {
	_, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	g := New(ctx)

	const groupNo = "avatar_disband_toctou_1"
	require.NoError(t, g.db.Insert(&Model{
		GroupNo: groupNo, Name: "已解散群", Creator: "c1",
		Status: GroupStatusDisband, Version: 100,
	}))

	text := "研发"
	affected, err := g.db.updateAvatarCustom(groupNo, &text, true, intPtr(3), false, 200)
	require.NoError(t, err)
	require.Equal(t, int64(0), affected, "已解散群必须命中 0 行")

	// 行未被改:avatar 列仍空、version 未 bump。
	got, err := g.db.QueryWithGroupNo(groupNo)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "", got.AvatarText, "已解散群的 avatar_text 不得被写入")
	require.Nil(t, got.AvatarColor, "已解散群的 avatar_color 不得被写入")
	require.Equal(t, int64(100), got.Version, "已解散群的 version 不得被 bump")

	// 正对照:未解散群正常命中 1 行并落库（version 每次都是新值，匹配行必然变更）。
	const liveNo = "avatar_disband_toctou_live_1"
	require.NoError(t, g.db.Insert(&Model{
		GroupNo: liveNo, Name: "正常群", Creator: "c1",
		Status: GroupStatusNormal, Version: 100,
	}))
	affected, err = g.db.updateAvatarCustom(liveNo, &text, true, intPtr(3), false, 200)
	require.NoError(t, err)
	require.Equal(t, int64(1), affected, "未解散群应命中 1 行")
	got, err = g.db.QueryWithGroupNo(liveNo)
	require.NoError(t, err)
	require.Equal(t, "研发", got.AvatarText)
	require.NotNil(t, got.AvatarColor)
	require.Equal(t, 3, *got.AvatarColor)
	require.Equal(t, int64(200), got.Version)
}

// TestGroupUpdateAvatarTextCleaned 回归 Fix3:存清洗后的文字(剔除不可见字符),避免
// 零宽/格式字符撑爆 avatar_text VARCHAR(16)。
func TestGroupUpdateAvatarTextCleaned(t *testing.T) {
	s, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	g := New(ctx)

	const groupNo = "avatar_upd_clean_1"
	seedCreatorGroup(t, g, groupNo, "清洗群")

	zwsp := string(rune(0x200B))
	// 2 个可见字 + 大量零宽字符:可见 rune 数 ≤4 过校验,但原始串远超 16 字符。
	padded := "研" + zwsp + "发" + zwsp + zwsp + zwsp + zwsp + zwsp + zwsp + zwsp + zwsp + zwsp + zwsp + zwsp + zwsp + zwsp + zwsp + zwsp + zwsp
	require.Equal(t, http.StatusOK, putGroupUpdate(t, s.GetRoute(), groupNo, map[string]string{
		attrKeyAvatarText: padded,
	}).Code)
	got, err := g.db.QueryWithGroupNo(groupNo)
	require.NoError(t, err)
	require.Equal(t, "研发", got.AvatarText, "应存清洗后的可见文字,不含零宽字符")
}
