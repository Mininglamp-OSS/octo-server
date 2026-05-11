package user

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// YUJ-413 Scope B · newChannelRespWithUserDetailResp extraMap 必须携带
// realname_verified / real_name / realname_verified_at 三个 key。
//
// 根因（YUJ-411 memory 07c6d080）：Android WKSDK 单用户 Channel 唯一数据源
// 是 /v1/channels/:id/:type → newChannelRespWithUserDetailResp，其结果
// 进 wkChannel.remoteExtraMap。气泡 fallback `from.remoteExtraMap[realname_verified]`
// 依赖此处下发；不加就永远读不到。
//
// 字段名与 friend/sync、conversation/sync、memberDetailResp、loginUserDetailResp
// 保持完全一致（snake_case）。

func TestNewChannelRespWithUserDetailResp_Verified_ExposesAllThreeFields(t *testing.T) {
	u := &UserDetailResp{
		UID:                "u-verified-1",
		Name:               "verified",
		RealnameVerified:   true,
		RealName:           "张三",
		RealnameVerifiedAt: 1778263617,
	}
	resp := newChannelRespWithUserDetailResp(u)
	if resp == nil {
		t.Fatal("resp nil")
	}
	if resp.Extra == nil {
		t.Fatal("Extra nil")
	}

	// 已实名：三字段齐全。
	assert.Equal(t, true, resp.Extra["realname_verified"])
	assert.Equal(t, "张三", resp.Extra["real_name"])
	assert.Equal(t, int64(1778263617), resp.Extra["realname_verified_at"])
}

func TestNewChannelRespWithUserDetailResp_Unverified_FalseButStillEmitsKey(t *testing.T) {
	u := &UserDetailResp{
		UID:              "u-unverified-1",
		Name:             "unverified",
		RealnameVerified: false,
	}
	resp := newChannelRespWithUserDetailResp(u)
	if resp == nil {
		t.Fatal("resp nil")
	}

	// 未实名：realname_verified=false **必须** 存在（客户端依赖这个 key 判定三态:
	// true / false / key缺失 表达 "已实名 / 明确未实名 / 数据未同步"）。
	// real_name 和 realname_verified_at 按 omitempty 语义省略（extraMap 上没有）。
	v, ok := resp.Extra["realname_verified"]
	assert.True(t, ok, "realname_verified key 必须存在（三态语义）")
	assert.Equal(t, false, v)

	_, hasRealName := resp.Extra["real_name"]
	assert.False(t, hasRealName, "未实名时 real_name 不应进 extraMap")

	_, hasTs := resp.Extra["realname_verified_at"]
	assert.False(t, hasTs, "未实名时 realname_verified_at 不应进 extraMap")
}
