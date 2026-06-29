package group

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/avatarrender"
	"github.com/stretchr/testify/require"
)

func intPtr(v int) *int { return &v }

// TestGroupRespExposesAvatarFields 回归 Fix4:GroupResp 暴露 avatar_text/avatar_color,
// 客户端 reload 后能读回已存的自定义值。
func TestGroupRespExposesAvatarFields(t *testing.T) {
	resp := (&GroupResp{}).fromModel(&Model{GroupNo: "g1", Name: "n", AvatarText: "研发", AvatarColor: intPtr(3)})
	require.Equal(t, "研发", resp.AvatarText)
	require.NotNil(t, resp.AvatarColor)
	require.Equal(t, 3, *resp.AvatarColor)

	// 未自定义 → 空串 / nil。
	plain := (&GroupResp{}).fromModel(&Model{GroupNo: "g2", Name: "n"})
	require.Equal(t, "", plain.AvatarText)
	require.Nil(t, plain.AvatarColor)
}

// TestGroupReqCheckAvatar 覆盖二次弹窗自定义头像参数的纯校验：avatar_text 去不可见
// 字符后最多 4 个 rune、avatar_color 必须为 nil 或落在色板下标区间，越界返回对应
// 字段名（供 Details.field）。两者均可选，缺省即“未自定义”。
func TestGroupReqCheckAvatar(t *testing.T) {
	n := avatarrender.PaletteSize()
	tests := []struct {
		name      string
		text      string
		color     *int
		wantField string
		wantOK    bool
	}{
		{"all default", "", nil, "", true},
		{"text only four cjk", "架构讨论", nil, "", true},
		{"text only four latin", "abcd", nil, "", true},
		{"color only zero", "", intPtr(0), "", true},
		{"color only max index", "", intPtr(n - 1), "", true},
		{"text and color", "研发", intPtr(3), "", true},
		{"text five cjk rejected", "后端架构讨论", nil, "avatar_text", false},
		{"text five latin rejected", "abcde", nil, "avatar_text", false},
		{"color negative rejected", "", intPtr(-1), "avatar_color", false},
		{"color out of range rejected", "", intPtr(n), "avatar_color", false},
		{"text invisible padding still ok", "  研 发  ", nil, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := groupReq{Members: []string{"u1"}, AvatarText: tt.text, AvatarColor: tt.color}
			field, ok := req.checkAvatar()
			require.Equal(t, tt.wantOK, ok, "ok mismatch")
			require.Equal(t, tt.wantField, field, "field mismatch")
		})
	}
}

// TestGroupAvatarCustomDBRoundTrip 覆盖自定义头像文字/颜色的落库读回：
//   - 默认建群（未自定义）写入 avatar_text=” / avatar_color=NULL（*int=nil），
//     这是渲染时回退“群名前 2 字（script 感知取字）+ ColorForSeed(group_no)”的哨兵；
//   - 显式自定义写入文字与色板下标，QueryWithGroupNo 原样读回。
//
// *int 的 nil→NULL 依赖 dbr Record() 对指针字段的处理（与 group_md *string 同理）。
func TestGroupAvatarCustomDBRoundTrip(t *testing.T) {
	_, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	g := New(ctx)

	// 默认：未自定义 → '' / NULL。
	const defaultGroupNo = "avatar_custom_default_1"
	require.NoError(t, g.db.Insert(&Model{
		GroupNo: defaultGroupNo,
		Name:    "default group",
		Creator: "creator_uid_1",
		Status:  1,
	}))
	got, err := g.db.QueryWithGroupNo(defaultGroupNo)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "", got.AvatarText)
	require.Nil(t, got.AvatarColor, "未自定义颜色必须读回 nil(NULL)，而非 0")

	// 自定义：文字 + 色板下标原样落库读回。
	const customGroupNo = "avatar_custom_set_1"
	require.NoError(t, g.db.Insert(&Model{
		GroupNo:     customGroupNo,
		Name:        "custom group",
		Creator:     "creator_uid_1",
		Status:      1,
		AvatarText:  "研发",
		AvatarColor: intPtr(3),
	}))
	got2, err := g.db.QueryWithGroupNo(customGroupNo)
	require.NoError(t, err)
	require.NotNil(t, got2)
	require.Equal(t, "研发", got2.AvatarText)
	require.NotNil(t, got2.AvatarColor)
	require.Equal(t, 3, *got2.AvatarColor)
}
