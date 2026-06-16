package message

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/stretchr/testify/assert"
)

// N0 安全收口验证门（YUJ-4667）：旧 /v1/message/search 读路径的
// filterResultsBySpace 此前 `default: append` 把无法识别的 channel_type 直接
// 放行，构成跨 Space 越权暴露面。本组用例锁定 fail-CLOSED 语义：
//   - 群聊(2)：仅命中 space 匹配的群；越权（B-only 群）剔除。
//   - DM(1)：仅命中 payload.space_id 匹配的；越权剔除。
//   - 未知 channel_type：一律剔除（核心安全收口，防回归）。

// fakeSpaceGroupService 只实现 filterResultsBySpace 调用到的 GetGroups。
type fakeSpaceGroupService struct {
	group.IService
	bySpace map[string]string // group_no -> space_id
}

func (f *fakeSpaceGroupService) GetGroups(groupNos []string) ([]*group.InfoResp, error) {
	out := make([]*group.InfoResp, 0, len(groupNos))
	for _, no := range groupNos {
		out = append(out, &group.InfoResp{GroupNo: no, SpaceID: f.bySpace[no]})
	}
	return out, nil
}

func newSpaceFilterTestMessage(gs group.IService) *Message {
	return &Message{
		Log:          log.NewTLog("space-filter-test"),
		groupService: gs,
	}
}

func dmResultWithSpace(channelID, spaceID string) map[string]interface{} {
	payload, _ := json.Marshal(map[string]interface{}{"space_id": spaceID})
	return map[string]interface{}{
		"channel_id":   channelID,
		"channel_type": float64(1),
		"payload":      base64.StdEncoding.EncodeToString(payload),
	}
}

func spaceFilterChannelIDs(results []map[string]interface{}) []string {
	out := make([]string, 0, len(results))
	for _, r := range results {
		id, _ := r["channel_id"].(string)
		out = append(out, id)
	}
	return out
}

// 场景：A 在 Space A 搜索，结果集混有 B-only 群、A 群、跨 Space DM、未知类型。
// fail-CLOSED 后只应保留属于 Space A 的群与 DM。
func TestFilterResultsBySpace_FailClosed(t *testing.T) {
	const spaceA = "spaceA"
	gs := &fakeSpaceGroupService{bySpace: map[string]string{
		"groupA": spaceA,
		"groupB": "spaceB",
	}}
	m := newSpaceFilterTestMessage(gs)

	results := []map[string]interface{}{
		{"channel_id": "groupA", "channel_type": float64(2)}, // 同 space 群 → 保留
		{"channel_id": "groupB", "channel_type": float64(2)}, // 越权群 → 剔除
		dmResultWithSpace("dmA", spaceA),                     // 同 space DM → 保留
		dmResultWithSpace("dmB", "spaceB"),                   // 越权 DM → 剔除
		// 未知 channel_type（系统/CS/社区等本期未开类型）→ 必须剔除（核心收口）。
		{"channel_id": "unknownChan", "channel_type": float64(99)},
	}

	filtered, err := m.filterResultsBySpace(results, spaceA)
	assert.NoError(t, err)

	ids := spaceFilterChannelIDs(filtered)
	assert.ElementsMatch(t, []string{"groupA", "dmA"}, ids,
		"fail-closed：仅保留同 Space 群/DM；越权频道与未知类型必须剔除")
	assert.NotContains(t, ids, "unknownChan",
		"未知 channel_type 必须 drop，绝不 fail-open 放行")
	assert.NotContains(t, ids, "groupB")
	assert.NotContains(t, ids, "dmB")
}

// 单独锁定「未知 channel_type 一律剔除」这一最高优先收口点，防止未来有人
// 误把 default 改回 append。
func TestFilterResultsBySpace_UnknownChannelTypeDropped(t *testing.T) {
	m := newSpaceFilterTestMessage(&fakeSpaceGroupService{})
	results := []map[string]interface{}{
		{"channel_id": "x", "channel_type": float64(0)},
		{"channel_id": "y", "channel_type": float64(3)},
		{"channel_id": "z", "channel_type": float64(255)},
	}
	filtered, err := m.filterResultsBySpace(results, "anySpace")
	assert.NoError(t, err)
	assert.Empty(t, filtered, "所有未知 channel_type 必须被 fail-closed 剔除")
}
