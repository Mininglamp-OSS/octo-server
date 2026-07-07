package cardtrust

import (
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/modules/incomingwebhook"
	"github.com/stretchr/testify/assert"
)

// 跨包常量一致性：本地复制的 iwh_ 前缀必须与 incomingwebhook 的导出契约常量
// 一致（生产代码不跨层 import modules/incomingwebhook —— 见其 display.go 顶注；
// 编译期不可见的漂移由本测试兜底）。
func TestWebhookPrefixConsistency(t *testing.T) {
	assert.Equal(t, incomingwebhook.WebhookIDPrefix, webhookIDPrefix)
}

// fakeRobot 是 robot.IService.ExistRobot 的测试替身，记录调用次数以验证缓存。
type fakeRobot struct {
	bots  map[string]bool
	err   error
	calls int
}

func (f *fakeRobot) ExistRobot(uid string) (bool, error) {
	f.calls++
	if f.err != nil {
		return false, f.err
	}
	return f.bots[uid], nil
}

// newTestResolver 绕过 robot.NewService，直接注入 fake（cardtrust.New 需要
// *config.Context，测试里只关心判定 + 缓存逻辑）。
func newTestResolver(t *testing.T, f *fakeRobot) *Resolver {
	t.Helper()
	c, err := lruNew(cacheCapacity)
	assert.NoError(t, err)
	return &Resolver{svc: f, cache: c, ttl: cacheTTL}
}

func TestTrustedWebhookPrefix(t *testing.T) {
	f := &fakeRobot{}
	r := newTestResolver(t, f)
	assert.True(t, r.Trusted("iwh_abc"), "webhook 合成身份可信")
	assert.Equal(t, 0, f.calls, "iwh_ 前缀不应查 robot 表")
}

func TestTrustedBotCached(t *testing.T) {
	f := &fakeRobot{bots: map[string]bool{"bot_x": true, "human_y": false}}
	r := newTestResolver(t, f)

	assert.True(t, r.Trusted("bot_x"))
	assert.True(t, r.Trusted("bot_x"), "第二次应命中缓存")
	assert.Equal(t, 1, f.calls, "同 uid 只查一次 robot 表(缓存生效)")

	assert.False(t, r.Trusted("human_y"), "非 bot 不可信")
	assert.False(t, r.Trusted("human_y"))
	assert.Equal(t, 2, f.calls, "否定裁决同样缓存")
}

func TestTrustedFailClosedNotCached(t *testing.T) {
	f := &fakeRobot{err: errors.New("db down")}
	r := newTestResolver(t, f)
	assert.False(t, r.Trusted("bot_z"), "查询失败 → fail-closed 不可信")
	// 错误裁决不得缓存：DB 恢复后应重新查询而非粘住 [卡片]
	f.err = nil
	f.bots = map[string]bool{"bot_z": true}
	assert.True(t, r.Trusted("bot_z"), "错误裁决不缓存,恢复后重查得到 true")
	assert.Equal(t, 2, f.calls)
}
