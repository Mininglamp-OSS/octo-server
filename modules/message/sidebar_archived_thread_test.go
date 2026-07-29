package message

// =============================================================================
// 归档子区在 sidebar 两个 tab 的可见性收口（task inactive-hiding-user-control / P0）
//
// 收敛到 /v1/conversation/sync 早已在跑的 status=active 语义。此前只有 XIN-1135
// 给 follow tab 的「自建豁免」补了 active 守卫，follow tab 常规路径与整个 recent
// tab 仍放行 archived，同一子区因此在移动端消失、在 Web 侧栏还在。
//
// 纯逻辑测试：dropArchivedThreadItems，无需 DB / IM。
// =============================================================================

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func threadItem(targetID string, status int) *SidebarItem {
	return &SidebarItem{
		TargetType:  int(common.ChannelTypeCommunityTopic),
		TargetID:    targetID,
		ChannelType: common.ChannelTypeCommunityTopic.Uint8(),
		ChannelID:   targetID,
		Status:      status,
	}
}

func itemIDs(items []*SidebarItem) []string {
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.TargetID)
	}
	return ids
}

func TestDropArchivedThreadItems_ArchivedDropped(t *testing.T) {
	items := []*SidebarItem{
		threadItem("g1____active", thread.ThreadStatusActive),
		threadItem("g1____archived", thread.ThreadStatusArchived),
	}
	assert.Equal(t, []string{"g1____active"}, itemIDs(dropArchivedThreadItems(items)))
}

// FAIL-OPEN：Status 未知（0）意味着 status 查询失败或行缺失。宁可短暂多显示，
// 也绝不因为一次查询抖动把用户的子区藏起来 —— 与 recent tab 既有降级方向一致。
func TestDropArchivedThreadItems_UnknownStatusKept(t *testing.T) {
	items := []*SidebarItem{threadItem("g1____unknown", 0)}
	assert.Equal(t, []string{"g1____unknown"}, itemIDs(dropArchivedThreadItems(items)),
		"status 未知必须保留（fail-open）")
}

// 归档判定只对子区条目成立：群/DM 即便 Status 字段偶然为 2 也不得被丢。
func TestDropArchivedThreadItems_NonThreadUntouched(t *testing.T) {
	items := []*SidebarItem{
		{
			TargetType:  int(common.ChannelTypeGroup),
			TargetID:    "g-normal",
			ChannelType: common.ChannelTypeGroup.Uint8(),
			Status:      thread.ThreadStatusArchived,
		},
		{
			TargetType:  int(common.ChannelTypePerson),
			TargetID:    "dm-normal",
			ChannelType: common.ChannelTypePerson.Uint8(),
			Status:      thread.ThreadStatusArchived,
		},
	}
	assert.Equal(t, []string{"g-normal", "dm-normal"}, itemIDs(dropArchivedThreadItems(items)),
		"非子区条目不参与归档判定")
}

// 归档非终态：复活（RecordMessageAndReactivate / 手动取消归档）后 status 回到
// active，同一条目必须在下一次 sync 重新可见。这是 P0 之后唯一的回归路径。
func TestDropArchivedThreadItems_ReactivatedBecomesVisibleAgain(t *testing.T) {
	archived := []*SidebarItem{threadItem("g1____t", thread.ThreadStatusArchived)}
	require.Empty(t, dropArchivedThreadItems(archived), "归档态不可见")

	revived := []*SidebarItem{threadItem("g1____t", thread.ThreadStatusActive)}
	assert.Equal(t, []string{"g1____t"}, itemIDs(dropArchivedThreadItems(revived)),
		"复活后必须重新出现")
}

func TestDropArchivedThreadItems_DoesNotMutateInput(t *testing.T) {
	items := []*SidebarItem{
		threadItem("g1____active", thread.ThreadStatusActive),
		threadItem("g1____archived", thread.ThreadStatusArchived),
	}
	_ = dropArchivedThreadItems(items)
	assert.Len(t, items, 2, "入参切片不得被修改")
	assert.Equal(t, "g1____archived", items[1].TargetID)
}

func TestDropArchivedThreadItems_Empty(t *testing.T) {
	assert.Empty(t, dropArchivedThreadItems(nil))
}
