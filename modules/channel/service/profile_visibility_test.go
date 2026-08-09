package service

import (
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/model"
	"github.com/stretchr/testify/assert"
)

// profile_visibility_test.go —— 共享可见性判定的契约测试。判定被 channelGet 与
// /v1/users/:uid 共用，任一分支放宽都会同时重开两个端点的越权面，故在此集中锁死。

func TestPersonProfileVisible_EarlyAllowIdentities(t *testing.T) {
	// checker 一旦被调用即 fail 测试：以下身份必须在触及共同群查询前就放行。
	mustNotCall := func(t *testing.T) CommonGroupChecker {
		return func(string, string) (bool, error) {
			t.Fatal("这些身份应在查询共同群之前就判定可见")
			return false, nil
		}
	}

	cases := []struct {
		name string
		in   PersonProfileInput
	}{
		{"self", PersonProfileInput{LoginUID: "u1", PeerUID: "u1"}},
		{"synthetic_identity", PersonProfileInput{LoginUID: "u1", PeerUID: "iwh_abc", SyntheticIdentity: true}},
		{"bot", PersonProfileInput{LoginUID: "u1", PeerUID: "u2", Robot: true}},
		{"system_account", PersonProfileInput{LoginUID: "u1", PeerUID: "u2", SystemAccount: true}},
		{"followed", PersonProfileInput{LoginUID: "u1", PeerUID: "u2", Followed: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, err := PersonProfileVisible(c.in, mustNotCall(t))
			assert.NoError(t, err)
			assert.True(t, ok)
		})
	}
}

// 无任何身份特权时才落到共同群查询，并如实透传其结果。
func TestPersonProfileVisible_FallsBackToCommonGroup(t *testing.T) {
	in := PersonProfileInput{LoginUID: "u1", PeerUID: "u2"}

	ok, err := PersonProfileVisible(in, func(a, b string) (bool, error) {
		assert.Equal(t, "u1", a)
		assert.Equal(t, "u2", b)
		return true, nil
	})
	assert.NoError(t, err)
	assert.True(t, ok, "有共同群 → 可见")

	ok, err = PersonProfileVisible(in, func(string, string) (bool, error) { return false, nil })
	assert.NoError(t, err)
	assert.False(t, ok, "无共同群 → 不可见（降级最小集）")
}

// 依赖缺失 / 查询出错必须 fail closed —— 不能因为拿不到关系就放开完整资料。
func TestPersonProfileVisible_FailsClosed(t *testing.T) {
	in := PersonProfileInput{LoginUID: "u1", PeerUID: "u2"}

	ok, err := PersonProfileVisible(in, nil)
	assert.NoError(t, err)
	assert.False(t, ok, "checker 未注入 → 不可见")

	ok, err = PersonProfileVisible(PersonProfileInput{LoginUID: "", PeerUID: "u2"},
		func(string, string) (bool, error) { return true, nil })
	assert.NoError(t, err)
	assert.False(t, ok, "调用方 uid 为空 → 不可见")

	ok, err = PersonProfileVisible(PersonProfileInput{LoginUID: "u1", PeerUID: ""},
		func(string, string) (bool, error) { return true, nil })
	assert.NoError(t, err)
	assert.False(t, ok, "目标 uid 为空 → 不可见")

	wantErr := errors.New("db down")
	ok, err = PersonProfileVisible(in, func(string, string) (bool, error) { return false, wantErr })
	assert.ErrorIs(t, err, wantErr, "查询错误必须上抛，由调用方决定 5xx，不可静默放行")
	assert.False(t, ok)
}

// 空 LoginUID 不得因 LoginUID == PeerUID 的字符串相等而被误判为"本人"。
func TestPersonProfileVisible_EmptyUIDsNotSelf(t *testing.T) {
	ok, err := PersonProfileVisible(PersonProfileInput{LoginUID: "", PeerUID: ""}, nil)
	assert.NoError(t, err)
	assert.False(t, ok)
}

// NewMinimalChannelResp 是白名单式构造：序列化后只有 channel/name/logo/robot 四个顶层
// 字段——零值字段（follow/status/notice/extra）也不得出现。
func TestNewMinimalChannelResp_WhitelistOnly(t *testing.T) {
	full := &model.ChannelResp{}
	full.Channel.ChannelID = "u1"
	full.Channel.ChannelType = 1
	full.Name = "N"
	full.Logo = "users/u1/avatar"
	full.Robot = 1
	full.LastOffline = 123
	full.DeviceFlag = 3
	full.Follow = 1
	full.Status = 2
	full.Notice = "secret-notice"
	full.Extra = map[string]interface{}{"short_no": "SNX"}

	m := NewMinimalChannelResp(full)
	assert.Equal(t, "u1", m.Channel.ChannelID)
	assert.Equal(t, uint8(1), m.Channel.ChannelType)
	assert.Equal(t, "N", m.Name)
	assert.Equal(t, "users/u1/avatar", m.Logo)
	assert.Equal(t, 1, m.Robot)
}
