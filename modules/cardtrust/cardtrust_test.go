package cardtrust

import (
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/modules/botidentity"
	"github.com/Mininglamp-OSS/octo-server/modules/incomingwebhook"
	"github.com/stretchr/testify/assert"
)

// 跨包常量一致性：本地复制的 iwh_ 前缀必须与 incomingwebhook 的导出契约常量
// 一致（生产代码不跨层 import modules/incomingwebhook —— 见其 display.go 顶注；
// 编译期不可见的漂移由本测试兜底）。
func TestWebhookPrefixConsistency(t *testing.T) {
	assert.Equal(t, incomingwebhook.WebhookIDPrefix, webhookIDPrefix)
}

// fakeBotIdentity 是统一 bot identity resolver 的测试替身，记录调用次数以验证缓存。
type fakeBotIdentity struct {
	kinds map[string]botidentity.Kind
	err   error
	calls int
}

func (f *fakeBotIdentity) Resolve(uid string) (*botidentity.Identity, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	kind, ok := f.kinds[uid]
	if !ok {
		return nil, nil
	}
	return &botidentity.Identity{UID: uid, Kind: kind}, nil
}

// newTestResolver 绕过 botidentity.New，直接注入 fake（cardtrust.New 需要
// *config.Context，测试里只关心判定 + 缓存逻辑）。
func newTestResolver(t *testing.T, f *fakeBotIdentity) *Resolver {
	t.Helper()
	c, err := lruNew(cacheCapacity)
	assert.NoError(t, err)
	return &Resolver{identity: f, cache: c, ttl: cacheTTL}
}

func TestTrustedWebhookPrefix(t *testing.T) {
	f := &fakeBotIdentity{}
	r := newTestResolver(t, f)
	assert.True(t, r.Trusted("iwh_abc"), "webhook 合成身份可信")
	assert.Equal(t, 0, f.calls, "iwh_ 前缀不应查询 bot identity")
}

func TestTrustedBotCached(t *testing.T) {
	f := &fakeBotIdentity{kinds: map[string]botidentity.Kind{
		"user_bot": botidentity.KindUserBot,
		"app_bot":  botidentity.KindAppBot,
	}}
	r := newTestResolver(t, f)

	assert.True(t, r.Trusted("user_bot"))
	assert.True(t, r.Trusted("user_bot"), "第二次应命中缓存")
	assert.Equal(t, 1, f.calls, "同 uid 只解析一次身份(缓存生效)")

	assert.True(t, r.Trusted("app_bot"), "published App Bot 可信")
	assert.True(t, r.Trusted("app_bot"), "App Bot 肯定裁决同样缓存")
	assert.Equal(t, 2, f.calls)

	assert.False(t, r.Trusted("human_y"), "非 bot 不可信")
	assert.False(t, r.Trusted("human_y"))
	assert.Equal(t, 3, f.calls, "否定裁决同样缓存")
}

func TestTrustedFailClosedNotCached(t *testing.T) {
	f := &fakeBotIdentity{err: errors.New("db down")}
	r := newTestResolver(t, f)
	assert.False(t, r.Trusted("bot_z"), "查询失败 → fail-closed 不可信")
	// 错误裁决不得缓存：DB 恢复后应重新查询而非粘住 [卡片]
	f.err = nil
	f.kinds = map[string]botidentity.Kind{"bot_z": botidentity.KindAppBot}
	assert.True(t, r.Trusted("bot_z"), "错误裁决不缓存,恢复后重查得到 true")
	assert.Equal(t, 2, f.calls)
}

func TestTrustedFailClosedForEmptyNilAndAmbiguousIdentity(t *testing.T) {
	var nilResolver *Resolver
	assert.False(t, nilResolver.Trusted("bot"))

	f := &fakeBotIdentity{err: botidentity.ErrAmbiguousIdentity}
	r := newTestResolver(t, f)
	assert.False(t, r.Trusted(""), "empty uid 不可信")
	assert.False(t, r.Trusted("ambiguous"), "跨表身份冲突必须 fail closed")
	assert.Equal(t, 1, f.calls, "empty uid 应在本地拒绝，不查询 resolver")
}
