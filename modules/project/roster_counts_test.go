package project

// D16 的三个计数字段，在**列表路由**上的口径。
//
// 详情路由的分裂由 TestCreateProjectSeatsTheCreatorsAgents 覆盖，列表路由一直没有用例。
//
// The list and detail routes must give the same roster-count meaning. The list read path now
// computes the split from its repeatable-read snapshot, while detail uses its own read snapshot;
// the observable contract is that both DTOs agree and the three counts add up.
//
// 三个字段的含义在 GA 前被改回来了：member_count 是全部席位（D16 之前的含义，也是
// modules/opanalytics 对同名字段的含义），人/分身的拆分放在 human_member_count 与
// agent_member_count 上。这个用例钉的就是这套口径，以及"三个数必须加得起来"。

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListRouteSplitsTheRosterAndTheThreeCountsAddUp(t *testing.T) {
	srv, p := setup(t)
	stubAllMemberGroup(t, "grp_counts_1") // registers its own t.Cleanup restore
	r := mountProject(t, p)
	_ = srv

	seedSpace(t, spaceA, 1)
	owner := seedUser(t, "u_cnt_owner")
	seedSpaceMember(t, spaceA, "u_cnt_owner", 0, 1)
	seedUser(t, "u_cnt_mate")
	seedSpaceMember(t, spaceA, "u_cnt_mate", 0, 1)
	seedAgent(t, spaceA, "bot_cnt_1", "u_cnt_owner", "octo_hosted")
	seedAgent(t, spaceA, "bot_cnt_2", "u_cnt_owner", "")

	w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", owner, map[string]any{
		"name":       "计数口径",
		"agent_uids": []string{"bot_cnt_1", "bot_cnt_2"},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	created := decodeResp(t, w)

	w = doOn(t, r, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add", owner,
		addMembersPayload("u_cnt_mate"))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// 两个人 + 两个分身。
	w = doOn(t, r, http.MethodGet, "/v1/space/"+spaceA+"/projects", owner, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var list []*Resp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
	var row *Resp
	for _, candidate := range list {
		if candidate.ProjectID == created.ProjectID {
			row = candidate
			break
		}
	}
	require.NotNil(t, row, "the created Project must be found by semantic ID")

	assert.Equal(t, 2, row.HumanMemberCount, "human_member_count 只算人")
	assert.Equal(t, 2, row.AgentMemberCount, "agent_member_count 只算分身席位")
	assert.Equal(t, 4, row.MemberCount,
		"member_count 是全部席位。它在 D16 里一度被改成只算人，而同一个服务的 "+
			"modules/opanalytics 用同名字段表示总数——一个服务两个含义，"+
			"客户端读错不会有任何东西报错。GA 前改回来是免费的")
	assert.Equal(t, row.HumanMemberCount+row.AgentMemberCount, row.MemberCount,
		"三个数必须加得起来。总数由两半推导，而不是再读第三个计数——那样它就会在另一个"+
			"读视图下算出来，并发写会让三个数互相矛盾")

	// 详情路由必须给出同一套答案，否则同一个字段在两个端点上又是两个意思。
	w = doOn(t, r, http.MethodGet, "/v1/projects/"+created.ProjectID, owner, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	detail := decodeResp(t, w)
	assert.Equal(t, row.MemberCount, detail.MemberCount, "列表与详情的 member_count 必须一致")
	assert.Equal(t, row.HumanMemberCount, detail.HumanMemberCount)
	assert.Equal(t, row.AgentMemberCount, detail.AgentMemberCount)
}
