package cardtrust

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-server/internal/resourceshare"
	"github.com/Mininglamp-OSS/octo-server/modules/botidentity"
	"github.com/Mininglamp-OSS/octo-server/modules/incomingwebhook"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/stretchr/testify/assert"
)

// 跨包常量一致性：本地复制的 iwh_ 前缀必须与 incomingwebhook 的导出契约常量
// 一致（生产代码不跨层 import modules/incomingwebhook —— 见其 display.go 顶注；
// 编译期不可见的漂移由本测试兜底）。
func TestWebhookPrefixConsistency(t *testing.T) {
	assert.Equal(t, incomingwebhook.WebhookIDPrefix, webhookIDPrefix)
}

func humanSharePayload(t *testing.T, target resourceshare.Target) ([]byte, *resourceshare.ProofVerifier) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	assert.NoError(t, err)
	signer, err := resourceshare.NewProofSigner(resourceshare.ProofSigningKey{KeyID: "proof-key-1", PrivateKey: key})
	assert.NoError(t, err)
	verifier, err := resourceshare.NewProofVerifier([]resourceshare.ProofVerificationKey{{
		KeyID: "proof-key-1", PublicKey: &key.PublicKey,
	}})
	assert.NoError(t, err)
	sealed, err := signer.Seal(map[string]interface{}{
		"type": cardmsg.InteractiveCard.Int(), "card_version": cardmsg.CardVersion, "profile": cardmsg.ProfileV1,
		"card": map[string]interface{}{
			"type": "AdaptiveCard", "version": cardmsg.CardVersion,
			"body": []interface{}{map[string]interface{}{"type": "TextBlock", "text": "Quarterly summary"}},
		},
	}, resourceshare.ProofContext{
		ActorUID: "human-a", SpaceID: "space-a", ProviderID: "smart-summary",
		Resource: resourceshare.ResourceRef{Type: "smart-summary", ID: "summary-1", Revision: "rev-1"},
		Target:   target, DeliveryID: strings.Repeat("a", 64),
	})
	assert.NoError(t, err)
	encoded, err := json.Marshal(sealed)
	assert.NoError(t, err)
	return encoded, verifier
}

func TestTrustedMessage_AllowsHumanOnlyWithContextBoundShareProof(t *testing.T) {
	target := resourceshare.Target{Kind: resourceshare.TargetGroup, GroupNo: "group-a"}
	payload, verifier := humanSharePayload(t, target)
	resolver := newTestResolver(t, &fakeBotIdentity{})
	resolver.proofVerifier = verifier
	observation := MessageObservation{
		FromUID: "human-a", ViewerUID: "human-b", SpaceID: "space-a",
		Target: target, Payload: payload,
	}

	assert.True(t, resolver.TrustedMessage(observation))
	mutations := []func(*MessageObservation){
		func(value *MessageObservation) { value.FromUID = "attacker" },
		func(value *MessageObservation) { value.SpaceID = "space-b" },
		func(value *MessageObservation) { value.Target.GroupNo = "group-b" },
		func(value *MessageObservation) { value.Payload = []byte(`{"type":17}`) },
	}
	for _, mutate := range mutations {
		changed := observation
		mutate(&changed)
		assert.False(t, resolver.TrustedMessage(changed))
	}
}

func TestTrustedMessage_PreservesBotAndWebhookTrustWithoutHumanProof(t *testing.T) {
	resolver := newTestResolver(t, &fakeBotIdentity{kinds: map[string]botidentity.Kind{
		"bot-a": botidentity.KindUserBot,
	}})
	for _, uid := range []string{"bot-a", "iwh_webhook"} {
		assert.True(t, resolver.TrustedMessage(MessageObservation{
			FromUID: uid, Payload: []byte(`{"type":17}`),
		}))
	}
	assert.False(t, resolver.TrustedMessage(MessageObservation{
		FromUID: "human-a", SpaceID: "space-a",
		Target:  resourceshare.Target{Kind: resourceshare.TargetDM, PeerUID: "human-b"},
		Payload: []byte(`{"type":17}`),
	}))
}

func TestMessageObservationFromChannelBuildsCanonicalTargets(t *testing.T) {
	tests := []struct {
		name        string
		channelID   string
		channelType uint8
		want        resourceshare.Target
		wantOK      bool
	}{
		{name: "dm", channelID: "human-b", channelType: common.ChannelTypePerson.Uint8(), want: resourceshare.Target{Kind: resourceshare.TargetDM, PeerUID: "human-b"}, wantOK: true},
		{name: "group", channelID: "group-a", channelType: common.ChannelTypeGroup.Uint8(), want: resourceshare.Target{Kind: resourceshare.TargetGroup, GroupNo: "group-a"}, wantOK: true},
		{name: "thread", channelID: "group-a____topic-a", channelType: common.ChannelTypeCommunityTopic.Uint8(), want: resourceshare.Target{Kind: resourceshare.TargetThread, GroupNo: "group-a", ShortID: "topic-a"}, wantOK: true},
		{name: "invalid", channelID: "group-a", channelType: 99, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, ok := TargetFromChannel(tt.channelID, tt.channelType)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, target)
		})
	}
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
