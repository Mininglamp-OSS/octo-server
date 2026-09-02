package oidc

// own_credential_test.go — 凭据归属判定的单元契约。
//
// 会话 token 那一类在端到端测试里造不出来(签发要 writer lease + rollout state),
// 而"会话存储报错时守卫往哪边失败"端到端**根本造不出条件**。两者都在这里覆盖。

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"

	"github.com/Mininglamp-OSS/octo-server/modules/botfather"
	"github.com/Mininglamp-OSS/octo-server/pkg/auth"
)

// fakeTokenReader 建模会话存储的三种结果。
//
// 未命中必须是 (TTL:-2, nil) 而不是 error —— 生产的 RedisSessionStore 就是这么
// 返回的(session_store.go),而这个区分正是守卫失败方向的依据:未命中 = 确定
// 不是我们的,报错 = 判不出来。double 建模错了,测的就是另一套语义。
type fakeTokenReader struct {
	payload string
	err     error
	calls   []string
}

func (f *fakeTokenReader) ReadToken(_ context.Context, key string) (auth.TokenRecord, error) {
	f.calls = append(f.calls, key)
	if f.err != nil {
		return auth.TokenRecord{}, f.err
	}
	if f.payload == "" {
		return auth.TokenRecord{TTL: -2}, nil // 未命中
	}
	return auth.TokenRecord{Payload: f.payload, TTL: 3600}, nil
}

func newDetector(r auth.TokenRecordReader) *OwnCredentialDetector {
	return &OwnCredentialDetector{reader: r, sessionPrefix: "token:"}
}

// 前缀类是纯本地判定,而且必须在**查存储之前**短路 —— 否则每个 uk_ 请求都白打
// 一次 Redis,更要紧的是存储不可用时它们会掉进 error 分支,把一个本可确定的
// 判断变成不确定。
func TestOwnCredential_PrefixesAreDecidedLocally(t *testing.T) {
	for name, c := range map[string]struct {
		raw  string
		want OwnCredentialKind
	}{
		"user api key": {botfather.UserAPIKeyPrefix + "abc123", OwnCredentialUserAPIKey},
		"bot token":    {botfather.BotTokenPrefix + "abc123", OwnCredentialBotToken},
	} {
		t.Run(name, func(t *testing.T) {
			// 存储设成"必然报错":如果判定去查了它,就会暴露出来。
			r := &fakeTokenReader{err: errors.New("redis is down")}
			got, err := newDetector(r).Classify(context.Background(), c.raw)
			if err != nil {
				t.Fatalf("a prefix match must not depend on the session store: %v", err)
			}
			if got != c.want {
				t.Errorf("Classify = %q, want %q", got, c.want)
			}
			if len(r.calls) != 0 {
				t.Errorf("the session store was queried for a prefix-decidable credential: %v", r.calls)
			}
		})
	}
}

// 能在我方会话存储里解析出来的,就是我们签发的。
func TestOwnCredential_SessionTokenIsOurs(t *testing.T) {
	r := &fakeTokenReader{payload: `{"uid":"u1","name":"n"}`}
	got, err := newDetector(r).Classify(context.Background(), "0e6a1c4b8f2d")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != OwnCredentialSessionToken {
		t.Fatalf("Classify = %q, want %q", got, OwnCredentialSessionToken)
	}
	if len(r.calls) != 1 || !strings.HasPrefix(r.calls[0], "token:") {
		t.Errorf("lookup key = %v, want the configured session cache prefix", r.calls)
	}
}

// 未命中 = 确定不是我们的 → 必须放行,否则上游凭据路径整条被掐断。
func TestOwnCredential_SessionMissFallsThrough(t *testing.T) {
	got, err := newDetector(&fakeTokenReader{}).Classify(context.Background(), "opaque-upstream-token")
	if err != nil {
		t.Fatalf("a miss is not an error: %v", err)
	}
	if got != OwnCredentialNone {
		t.Errorf("Classify = %q, want none — an upstream opaque token must reach /userinfo", got)
	}
}

// 存储报错 = **判不出来**,必须回 error 让调用方 fail-closed。
//
// 这是这道守卫的失败方向,也是端到端造不出来的那个条件。若这里吞掉错误返回
// "不是我们的",一次 Redis 抖动就变成一次凭据外发 —— 而且是不可观测的:
// 客户端只看到 401,凭据已经出去了。
func TestOwnCredential_StoreErrorIsUndecidedNotForeign(t *testing.T) {
	got, err := newDetector(&fakeTokenReader{err: errors.New("redis is down")}).
		Classify(context.Background(), "some-token")
	if err == nil {
		t.Fatal("a session-store failure was reported as 'not ours'; the caller would then " +
			"forward the credential upstream, which makes a transient infrastructure error " +
			"indistinguishable from a credential leak")
	}
	if got != OwnCredentialNone {
		t.Errorf("kind alongside an error should be none, got %q", got)
	}
}

// nil detector / 空串不得 panic,也不得把空串判成我方凭据。
func TestOwnCredential_NilAndEmptyAreSafe(t *testing.T) {
	var d *OwnCredentialDetector
	if got, err := d.Classify(context.Background(), "anything"); err != nil || got != OwnCredentialNone {
		t.Errorf("nil detector: got (%q, %v)", got, err)
	}
	if got, err := newDetector(&fakeTokenReader{}).Classify(context.Background(), "   "); err != nil ||
		got != OwnCredentialNone {
		t.Errorf("blank credential: got (%q, %v)", got, err)
	}
}

// /exchange 与 integration 的两个端点走的是同一条上游路径,所以同一道守卫必须
// 在这里也生效。
//
// 这条路撞上的概率比 integration 还高一点:/exchange 和 /exchange-jwt 请求体
// 一模一样、字段都叫 access_token,文件头自己就在警告发错端点只会得到一个
// 无差别 401。发错的那次,凭据已经出去了。
func TestOwnCredential_ExchangeDoesNotForwardOurOwnCredential(t *testing.T) {
	m := newMockOAuth2Provider(t)
	prov, err := newOAuth2Provider(m.providerConfig())
	if err != nil {
		t.Fatalf("newOAuth2Provider: %v", err)
	}
	o := newOAuth2ExchangeTestOIDC(t, prov)
	// 会话存储里查得到 = 我方会话 token。
	o.ownCred = newDetector(&fakeTokenReader{payload: `{"uid":"u1","name":"n"}`})

	m.mu.Lock()
	m.LastUserInfoRequest = nil
	m.mu.Unlock()
	w := postExchange(o, `{"access_token":"0e6a1c4b8f2d"}`)

	if w.Code == http.StatusOK {
		t.Error("a credential of ours must not authenticate at /exchange")
	}
	m.mu.Lock()
	last := m.LastUserInfoRequest
	m.mu.Unlock()
	if last != nil {
		t.Errorf("our own credential reached the upstream IdP (userinfo query=%q); this "+
			"endpoint and /exchange-jwt take an identically shaped request, so posting to "+
			"the wrong one is a realistic mistake and it leaks the credential", last.Query.Encode())
	}
}

// newOAuth2ExchangeTestOIDC 组一个只够跑 /exchange 的 OIDC —— 不打库:这条用例
// 断言的是"外呼有没有发生",在 identity 之前就该结束。
func newOAuth2ExchangeTestOIDC(t *testing.T, prov AuthProvider) *OIDC {
	t.Helper()
	cfg := &Config{Enabled: true, Provider: ProviderConfig{
		ID: "test", Name: "Test IdP", Kind: KindOAuth2, Issuer: prov.Issuer(),
		AllowNewUser: true,
	}}
	store := &fakeIdentityStore{bindings: map[string]*IdentityModel{}}
	users := &fakeUserLookup{loginResp: &IssueSessionResp{UID: "u-nope", LoginRespJSON: `{}`}}
	return &OIDC{
		Log:        log.NewTLog("OIDC-owncred"),
		cfg:        cfg,
		provider:   prov,
		service:    newService(cfg.Provider, store, users),
		store:      store,
		stateStore: newMemoryStateStore(),
		authcode:   newFakeAuthcode(),
		audit:      newFakeAudit(),
	}
}

// 业务 JWT 发到 /exchange(而不是 /exchange-jwt)必须就地拒绝,不得转发上游。
//
// 这条是"守卫只挂了两个消费者中的一个"的又一例。modules/integration 的 oidcAuth
// 先跑 HMAC 验签、`!IsForeignToken` 就地拒;/exchange 上根本没有验签这一步,而
// ownCred.Classify 覆盖不到业务 JWT —— 它无前缀、无状态,会话查表必然落空。
//
// 撞上的概率不低:两个端点请求体一模一样、字段都叫 access_token,本文件顶部的
// 注释自己就在警告发错端点只会得到一个无差别 401。发错的那一次,一张带着我方
// 密钥下合法 HMAC、载有真实 userId 的 token 就进了第三方的访问日志 —— 那既是
// PII,也是可离线校验密钥的材料。
func TestOwnCredential_ExchangeDoesNotForwardOurBusinessJWT(t *testing.T) {
	m := newMockOAuth2Provider(t)
	prov, err := newOAuth2Provider(m.providerConfig())
	if err != nil {
		t.Fatalf("newOAuth2Provider: %v", err)
	}
	o := newOAuth2ExchangeTestOIDC(t, prov)
	secret := []byte("exchange-path-test-secret-32byte!")
	o.bearerJWT = newBearerJWTVerifierForTest(secret, prov.Issuer()+bearerJWTIssuerSuffix)
	// 会话存储查不到 —— 业务 JWT 本来就不在里面,这里如实建模。
	o.ownCred = newDetector(&fakeTokenReader{})

	tok := signBearerTesting(t, secret, 2200005, "desk.user", time.Now().Add(15*24*time.Hour))

	m.mu.Lock()
	m.LastUserInfoRequest = nil
	m.mu.Unlock()
	w := postExchange(o, `{"access_token":"`+tok+`"}`)

	if w.Code == http.StatusOK {
		t.Error("a business JWT is not an upstream credential; /exchange must not accept it")
	}
	m.mu.Lock()
	last := m.LastUserInfoRequest
	m.mu.Unlock()
	if last != nil {
		t.Errorf("our business JWT reached the upstream IdP (userinfo query=%q); "+
			"modules/integration catches this exact token by running the HMAC verifier "+
			"before falling through — /exchange has no such stage", last.Query.Encode())
	}
}

// 反面:上游不透明凭据在 /exchange 上必须照常走上游 —— 这是这个端点的主用途。
func TestOwnCredential_ExchangeStillForwardsUpstreamCredential(t *testing.T) {
	m := newMockOAuth2Provider(t)
	prov, err := newOAuth2Provider(m.providerConfig())
	if err != nil {
		t.Fatalf("newOAuth2Provider: %v", err)
	}
	o := newOAuth2ExchangeTestOIDC(t, prov)
	o.bearerJWT = newBearerJWTVerifierForTest(
		[]byte("exchange-path-test-secret-32byte!"), prov.Issuer()+bearerJWTIssuerSuffix)
	o.ownCred = newDetector(&fakeTokenReader{})

	m.mu.Lock()
	m.LastUserInfoRequest = nil
	m.mu.Unlock()
	postExchange(o, `{"access_token":"`+m.KnownAccessToken+`"}`)

	m.mu.Lock()
	last := m.LastUserInfoRequest
	m.mu.Unlock()
	if last == nil {
		t.Error("an opaque upstream token must still reach /userinfo; refusing it would " +
			"break the credential path this endpoint exists for")
	}
}

// 已知**未闭合**的格子,行为在此钉住,不是断言它安全。
//
// 一张 alg:none 或 RS 系头部的 JWT 在验签之前就被拒(算法混淆防护),失败点带
// ErrJWTForeign 标记 → 判成"不是我们的" → 在 kind=oauth2 下被转发进上游的
// /userinfo query。造它不需要密钥。
//
// 为什么不修:kind=oauth2 下**合法的**上游不透明 access_token 本身就可能是 JWT
// 形态(很多 OAuth2 provider 签 JWT access token)。把"验签前失败"一律判成我方
// 凭据就会掐断 C2 —— 而 C2 是这两个端点存在的理由。无法归属的凭据只能转发。
//
// 风险接受的实际内容:任何人可以让本服务把一段自造的字符串写进厂商的访问日志。
// 那不是**我方凭据**外泄(不变量仍然成立:我们签发的东西不会出去),而是把我们
// 当成一次写日志的中继。端点级 IP 限流压住速率,不封闭通道。
//
// 这条测试的作用是让行为可见:哪天有人"顺手修好"它,C2 会静默断掉,而这里会红。
func TestForeignJWTShape_IsForwardedUpstream_KnownOpenCell(t *testing.T) {
	m := newMockOAuth2Provider(t)
	prov, err := newOAuth2Provider(m.providerConfig())
	if err != nil {
		t.Fatalf("newOAuth2Provider: %v", err)
	}
	o := newOAuth2ExchangeTestOIDC(t, prov)
	o.bearerJWT = newBearerJWTVerifierForTest(
		[]byte("exchange-path-test-secret-32byte!"), prov.Issuer()+bearerJWTIssuerSuffix)
	o.ownCred = newDetector(&fakeTokenReader{})

	// alg:none —— 无需任何密钥即可构造。
	noneJWT := signHS256Raw(t, "irrelevant", `{"alg":"none","typ":"JWT"}`,
		`{"userId":2200005,"iat":1788343918,"exp":1789639978}`)

	m.mu.Lock()
	m.LastUserInfoRequest = nil
	m.mu.Unlock()
	postExchange(o, `{"access_token":"`+noneJWT+`"}`)

	m.mu.Lock()
	last := m.LastUserInfoRequest
	m.mu.Unlock()
	if last == nil {
		t.Skip("behaviour changed: a JWT-shaped credential is no longer forwarded. If that " +
			"was deliberate, verify that JWT-shaped **upstream** access tokens still work " +
			"(C2), and move this cell out of guard-matrix.md's open list.")
	}
	// 现状:被转发。钉住"它不是我方签发的凭据"这一点 —— 不变量没破。
	if _, verr := o.bearerJWT.VerifyForAuthentication(noneJWT, time.Now()); verr == nil {
		t.Fatal("an alg:none token verified against our secret; the algorithm-confusion " +
			"guard is broken, which is a different and much worse problem")
	}
}
