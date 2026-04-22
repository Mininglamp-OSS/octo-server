package webhook

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/stretchr/testify/assert"
)

func users(uid string, notice int) []*user.Resp {
	return []*user.Resp{{UID: uid, NewMsgNotice: notice}}
}

// 群 mute=1 且无子区 setting → 跳过推送(降级:父群静音)
func TestAllowPush_ThreadFallbackToGroupMute(t *testing.T) {
	w := &Webhook{}
	us := users("u1", 1)
	groupSettings := []*group.SettingResp{{UID: "u1", Mute: 1}}
	var threadSettings []*threadSettingResp // 空

	result := w.allowPush(us, nil, groupSettings, threadSettings, "u1", "")
	assert.False(t, result, "父群免打扰应跳过子区推送")
}

// 子区 mute=0 覆盖群 mute=1 → 允许推送
func TestAllowPush_ThreadSettingOverridesGroupMute(t *testing.T) {
	w := &Webhook{}
	us := users("u1", 1)
	groupSettings := []*group.SettingResp{{UID: "u1", Mute: 1}}
	threadSettings := []*threadSettingResp{{UID: "u1", Mute: 0}}

	result := w.allowPush(us, nil, groupSettings, threadSettings, "u1", "")
	assert.True(t, result, "子区显式 mute=0 应覆盖父群 mute=1")
}

// 子区 mute=1 覆盖群 mute=0 → 跳过推送
func TestAllowPush_ThreadMuteOverridesGroupUnmute(t *testing.T) {
	w := &Webhook{}
	us := users("u1", 1)
	groupSettings := []*group.SettingResp{{UID: "u1", Mute: 0}}
	threadSettings := []*threadSettingResp{{UID: "u1", Mute: 1}}

	result := w.allowPush(us, nil, groupSettings, threadSettings, "u1", "")
	assert.False(t, result, "子区显式 mute=1 应覆盖父群 mute=0")
}

// 都没设置 → 允许推送
func TestAllowPush_NoMute(t *testing.T) {
	w := &Webhook{}
	us := users("u1", 1)

	result := w.allowPush(us, nil, nil, nil, "u1", "")
	assert.True(t, result)
}

// 用户全局 NewMsgNotice=0 → 跳过推送(无论群/子区设置)
func TestAllowPush_UserGlobalOff(t *testing.T) {
	w := &Webhook{}
	us := users("u1", 0)
	threadSettings := []*threadSettingResp{{UID: "u1", Mute: 0}}

	result := w.allowPush(us, nil, nil, threadSettings, "u1", "")
	assert.False(t, result, "用户关闭全局通知应跳过,不受子区设置影响")
}
