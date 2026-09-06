package oidc

// redemption_ledger_test.go — 兑换台账的准入判定。
//
// 这些用例钉的是**准入边界**,不是验签:一张 token 能不能通过验签由
// bearer_jwt_test.go 覆盖,能不能用它换一个新会话在这里。

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
)

// ---- 策略取值 ---------------------------------------------------------------

// 非正值必须回落默认值。F=0 的字面语义是"所有首次兑换都太晚",即全员登录失败 ——
// 一个漏配的 env 不该有这种后果。
func TestRedemptionPolicy_NonPositiveValuesFallBackToDefaults(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   redemptionPolicy
	}{
		{"zero value", redemptionPolicy{}},
		{"negative", redemptionPolicy{firstRedeemMaxAge: -time.Hour, idleWindow: -time.Hour}},
		{"only F set", redemptionPolicy{firstRedeemMaxAge: time.Hour}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.normalized()
			if got.firstRedeemMaxAge <= 0 || got.idleWindow <= 0 {
				t.Fatalf("normalized() = %+v, both bounds must be positive", got)
			}
			if tc.in.firstRedeemMaxAge > 0 && got.firstRedeemMaxAge != tc.in.firstRedeemMaxAge {
				t.Errorf("a configured F was overwritten: got %v, want %v",
					got.firstRedeemMaxAge, tc.in.firstRedeemMaxAge)
			}
		})
	}
}

func TestLoadRedemptionPolicy_ReadsEnvAndFallsBackOnGarbage(t *testing.T) {
	t.Setenv("OCTO_OIDC_BEARER_JWT_FIRST_REDEEM_MAX_AGE", "90m")
	t.Setenv("OCTO_OIDC_BEARER_JWT_REDEEM_IDLE_WINDOW", "not-a-duration")
	p := loadRedemptionPolicy()
	if p.firstRedeemMaxAge != 90*time.Minute {
		t.Errorf("F = %v, want 90m from the env", p.firstRedeemMaxAge)
	}
	if p.idleWindow != defaultRedeemIdleWindow {
		t.Errorf("T = %v, want the default after an unparseable value; a typo in one knob "+
			"must not disable the other", p.idleWindow)
	}
}

// 亚秒取值必须被收敛掉。判定在 Lua 里按整秒比较,`int64(500ms/time.Second)` 是 0,
// 于是脚本比的是 `now - iat > 0` —— 任何一秒前签发的 token 都被拒。这与 F=0 是
// 同一个全员登录失败,只是从另一扇门进来。
func TestRedemptionPolicy_SubSecondBoundsCannotTruncateToZero(t *testing.T) {
	for _, in := range []time.Duration{time.Millisecond, 500 * time.Millisecond, 999 * time.Millisecond} {
		p := redemptionPolicy{firstRedeemMaxAge: in, idleWindow: in}.normalized()
		if int64(p.firstRedeemMaxAge/time.Second) < 1 || int64(p.idleWindow/time.Second) < 1 {
			t.Fatalf("policy %v normalized to %+v: it reaches the Lua script as whole "+
				"seconds, and 0 there rejects every redemption", in, p)
		}
	}
	// 非整秒取值也要截断,让两条路径至少**用同一个边界**。
	//
	// 说清楚它没解决什么:两边比较的精度仍不同 —— Lua 比整秒,降级路径比
	// time.Duration,所以在 F 附近一秒内的 token 两条路可能给出不同结论。在 24
	// 小时量级的边界上这没有意义,但"完全一致"是句过头的话。
	p := redemptionPolicy{firstRedeemMaxAge: 90*time.Second + 400*time.Millisecond}.normalized()
	if p.firstRedeemMaxAge != 90*time.Second {
		t.Errorf("F = %v, want it truncated to whole seconds so both paths use the same "+
			"bound", p.firstRedeemMaxAge)
	}
}

// T 长过记录能存活的时间就是不可执行的:记录先没了,超时的兑换会以"首次兑换超过
// F"的名义被拒 —— 拒得对,归因是错的,运维会去调 F。收敛到能执行的值,启动日志
// 打印的也就是真正生效的值。
func TestRedemptionPolicy_NeitherBoundCanExceedTheRecordLifetime(t *testing.T) {
	p := redemptionPolicy{
		firstRedeemMaxAge: 60 * 24 * time.Hour,
		idleWindow:        60 * 24 * time.Hour,
	}.normalized()

	if p.idleWindow != redemptionRecordMaxTTL {
		t.Errorf("T = %v, want it capped at %v — a record cannot outlive that, so a longer "+
			"window can never fire", p.idleWindow, redemptionRecordMaxTTL)
	}
	// F 长过记录寿命会**反过来废掉 T**:记录在 maxTTL 被截掉,之后的兑换找不到
	// 记录、被当成首次,而它仍在 F 之内于是放行 —— 空闲窗口就这样被绕过去了。
	if p.firstRedeemMaxAge != redemptionRecordMaxTTL {
		t.Errorf("F = %v, want it capped at %v — an F longer than we remember the token "+
			"turns every post-eviction redemption into an admitted 'first' one, which "+
			"defeats T entirely", p.firstRedeemMaxAge, redemptionRecordMaxTTL)
	}
	if got := recordTTL(time.Now().Add(365*24*time.Hour), time.Now()); got < p.idleWindow {
		t.Errorf("record ttl %v < T %v: the window would never be reachable", got, p.idleWindow)
	}
}

// 默认 F 必须够宽,能覆盖"登录后过一会儿才兑换"。
//
// 这条钉的是**数值本身**,不是接线:线上那个 401 就是 F=10min 造成的,而所有
// handler 级用例都把台账 stub 掉了 —— 把默认值改回 10 分钟,它们全都照样绿。
func TestDefaultFirstRedeemMaxAge_IsWideEnoughForALateClient(t *testing.T) {
	if defaultFirstRedeemMaxAge < time.Hour {
		t.Fatalf("default F = %v; the production failure this task exists to fix was a "+
			"36-minute-old token being refused, so anything at that scale reinstates it",
			defaultFirstRedeemMaxAge)
	}
	// 同时确认默认组合下降级路径的上限就是 F(min(F,T) 取到 F)。
	if defaultRedeemIdleWindow < defaultFirstRedeemMaxAge {
		t.Errorf("default T (%v) < default F (%v): the fallback bound would drop to T, "+
			"which is not what the defaults are documented to do",
			defaultRedeemIdleWindow, defaultFirstRedeemMaxAge)
	}
}

// 台账拿到的必须是收敛后的取值 —— 否则配置里的亚秒值会绕过 normalized 直接进
// 脚本。这条同时钉住"两处实现同一条规则"的接缝。
func TestNewRedisRedemptionLedger_StoresTheNormalizedPolicy(t *testing.T) {
	l := &redisRedemptionLedger{policy: redemptionPolicy{
		firstRedeemMaxAge: 500 * time.Millisecond,
		idleWindow:        60 * 24 * time.Hour,
	}.normalized()}
	if l.policy.firstRedeemMaxAge < time.Second || l.policy.idleWindow > redemptionRecordMaxTTL {
		t.Fatalf("ledger policy = %+v, want the normalized bounds", l.policy)
	}
}

// admitted() 是唯一的"放行"判据。新增一个 outcome 却忘了在这里归类,默认就是拒绝
// —— 这个方向是对的(fail-closed),但要有用例把两边都钉住。
func TestRedemptionOutcome_OnlyTheAdmitOutcomesAdmit(t *testing.T) {
	admit := map[redemptionOutcome]bool{
		redeemAdmitFirst:        true,
		redeemAdmitRepeat:       true,
		redeemDegradedAdmit:     true,
		redeemUnconfiguredAdmit: true,
	}
	for _, l := range redemptionOutcomeLabels() {
		out := redemptionOutcome(l)
		if got, want := out.admitted(), admit[out]; got != want {
			t.Errorf("%s.admitted() = %v, want %v", l, got, want)
		}
	}
	if redemptionOutcome("something-new").admitted() {
		t.Error("an unknown outcome must not admit: the fail-open direction here mints a session")
	}
}

// ---- 降级判定 ---------------------------------------------------------------

// 拿不到台账时按 iat 判一个上限,而且必须比"只看 exp"紧:Redis 故障不能顺带把
// 重放窗口放大到 token 自己的 15 天。
func TestFallbackAdmits_KeepsACeiling(t *testing.T) {
	now := time.Now()
	p := redemptionPolicy{firstRedeemMaxAge: time.Hour, idleWindow: 7 * 24 * time.Hour}

	if !p.fallbackAdmits(now.Add(-30*time.Minute), now) {
		t.Error("a token issued 30m ago must be admitted; a Redis blip is not a reason to " +
			"refuse someone who just authenticated")
	}
	// exp 还有 5 天,但 iat 已是 10 天前 —— 降级路径也必须拒。
	if p.fallbackAdmits(now.Add(-10*24*time.Hour), now) {
		t.Error("a 10-day-old token was admitted; degrading must not widen the window to " +
			"the token's own exp")
	}
}

// 降级上限取 min(F, T)。T 配得比 F 短是合法配置("首次可以来得晚,复用必须频繁"),
// 而只用 F 会让 Redis 故障期间**比正常时更好过** —— 两小时前用过的 token 正常是
// reject_idle,降级却放行。降级路径的正当性全靠"绝不比正常路径松"。
func TestFallbackAdmits_NeverLooserThanTheIdleWindow(t *testing.T) {
	now := time.Now()
	p := redemptionPolicy{firstRedeemMaxAge: 24 * time.Hour, idleWindow: time.Hour}

	if p.fallbackAdmits(now.Add(-2*time.Hour), now) {
		t.Errorf("a token 2h old was admitted with F=24h, T=1h; the ledger would have "+
			"refused it as idle, so the fallback bound must be min(F,T)=%v", time.Hour)
	}
	if !p.fallbackAdmits(now.Add(-30*time.Minute), now) {
		t.Error("30m is inside both bounds and must still be admitted")
	}
}

// ---- OIDC.admitRedemption 的接线 ---------------------------------------------

type fakeRedemptionLedger struct {
	outcome    redemptionOutcome
	err        error
	calls      int
	lastDigest string
}

func (f *fakeRedemptionLedger) Admit(_ context.Context, digest string, _, _, _ time.Time) (redemptionOutcome, error) {
	f.calls++
	f.lastDigest = digest
	return f.outcome, f.err
}

func newRedeemTestOIDC(led redemptionLedger, p redemptionPolicy) *OIDC {
	return &OIDC{Log: log.NewTLog("OIDC-test"), redeemLedger: led, redeemPolicy: p}
}

func TestAdmitRedemption_UsesTheLedgerVerdict(t *testing.T) {
	now := time.Now()
	led := &fakeRedemptionLedger{outcome: redeemAdmitRepeat}
	o := newRedeemTestOIDC(led, redemptionPolicy{})
	rj := &RedeemedBearerJWT{IssuedAt: now.Add(-10 * time.Minute), ExpiresAt: now.Add(24 * time.Hour)}

	if got := o.admitRedemption(context.Background(), "tok", rj, now, "trace"); got != redeemAdmitRepeat {
		t.Errorf("outcome = %s, want %s", got, redeemAdmitRepeat)
	}
	if led.calls != 1 {
		t.Errorf("ledger calls = %d, want 1", led.calls)
	}
	if led.lastDigest == "tok" || len(led.lastDigest) != 64 {
		t.Errorf("digest = %q, want the token's sha256 hex, never the token itself", led.lastDigest)
	}
}

// 台账报错 = 拿不到历史。不能因此无条件放行,也不能因此拒绝一个刚签发的 token。
func TestAdmitRedemption_LedgerErrorFallsBackToTheCeiling(t *testing.T) {
	now := time.Now()
	p := redemptionPolicy{firstRedeemMaxAge: time.Hour, idleWindow: 7 * 24 * time.Hour}
	led := &fakeRedemptionLedger{err: errors.New("redis: connection refused")}
	o := newRedeemTestOIDC(led, p)

	fresh := &RedeemedBearerJWT{IssuedAt: now.Add(-5 * time.Minute), ExpiresAt: now.Add(24 * time.Hour)}
	if got := o.admitRedemption(context.Background(), "tok", fresh, now, "trace"); got != redeemDegradedAdmit {
		t.Errorf("fresh token during an outage = %s, want %s; a Redis blip must not "+
			"become a login outage", got, redeemDegradedAdmit)
	}
	stale := &RedeemedBearerJWT{IssuedAt: now.Add(-10 * 24 * time.Hour), ExpiresAt: now.Add(24 * time.Hour)}
	if got := o.admitRedemption(context.Background(), "tok", stale, now, "trace"); got != redeemDegradedReject {
		t.Errorf("stale token during an outage = %s, want %s", got, redeemDegradedReject)
	}
}

// 未配置台账走同一条降级路径 —— 对这次请求而言"没有台账"和"台账挂了"是同一件事。
func TestAdmitRedemption_NilLedgerDoesNotFailOpen(t *testing.T) {
	now := time.Now()
	o := newRedeemTestOIDC(nil, redemptionPolicy{})
	stale := &RedeemedBearerJWT{IssuedAt: now.Add(-30 * 24 * time.Hour), ExpiresAt: now.Add(24 * time.Hour)}
	got := o.admitRedemption(context.Background(), "tok", stale, now, "trace")
	if got.admitted() {
		t.Errorf("outcome = %s; without a ledger the default must still be the ceiling, "+
			"not 'anything within exp'", got)
	}
	// 未配置与 Redis 故障必须能分开:前者不会自愈,T 永远不生效;后者会。共用一组
	// label 的话,一次接线回归在看板上只会显示成"Redis 在抖"。
	if got != redeemUnconfiguredReject {
		t.Errorf("outcome = %s, want %s: a missing ledger is not a Redis outage",
			got, redeemUnconfiguredReject)
	}
}

func TestAdmitRedemption_NilCredentialIsRefused(t *testing.T) {
	o := newRedeemTestOIDC(&fakeRedemptionLedger{outcome: redeemAdmitFirst}, redemptionPolicy{})
	if got := o.admitRedemption(context.Background(), "tok", nil, time.Now(), "trace"); got.admitted() {
		t.Errorf("outcome = %s, want a refusal: the fail-open branch here issues a session", got)
	}
}

// ---- key / TTL 形状 ----------------------------------------------------------

func TestRedemptionDigest_NeverCarriesTheToken(t *testing.T) {
	tok := "header.payload.signature-that-is-a-live-credential"
	d := redemptionDigest(tok)
	if strings.Contains(d, "signature") || strings.Contains(redemptionKey(d), tok) {
		t.Fatalf("digest/key leaked the token: %q", redemptionKey(d))
	}
	if d != redemptionDigest(tok) {
		t.Error("digest must be stable: an unstable key means every redemption looks like the first")
	}
	if d == redemptionDigest(tok+"x") {
		t.Error("two different tokens collided")
	}
}

// 记录必须活到 token 自己的 exp,不是活到 T —— 若记录先于 token 消失,重复兑换的
// 客户端会被当成"首次兑换"。上限只防 exp 离谱的 token 占用近乎永久的记录。
func TestRecordTTL_TracksTheTokenLifetimeWithACap(t *testing.T) {
	now := time.Now()
	if got := recordTTL(now.Add(3*24*time.Hour), now); got != 3*24*time.Hour {
		t.Errorf("ttl = %v, want the token's remaining life (3d)", got)
	}
	if got := recordTTL(now.Add(365*24*time.Hour), now); got != redemptionRecordMaxTTL {
		t.Errorf("ttl = %v, want it capped at %v", got, redemptionRecordMaxTTL)
	}
	if got := recordTTL(now.Add(-time.Hour), now); got < time.Second {
		t.Errorf("ttl = %v, want a positive floor; Redis rejects a non-positive EX", got)
	}
}

// ---- handler 行为 ------------------------------------------------------------

// 回归:线上遇到的那一个。登录后 36 分钟才首次兑换,以前被 10 分钟的 iat 上限拒,
// 现在必须成功。
func TestExchangeJWT_FirstRedemptionLongAfterIssuanceSucceeds(t *testing.T) {
	users := &fakeUserLookup{
		loginResp: &IssueSessionResp{
			UID:           "uid-late",
			LoginRespJSON: `{"token":"sess-late","uid":"uid-late"}`,
		},
	}
	o := newTestOIDCForBearerJWT(t, []byte("s"), "https://idp-test.example.com#bearer-jwt",
		users, newFakeIdentityStore())
	o.redeemLedger = &fakeRedemptionLedger{outcome: redeemAdmitFirst}

	now := time.Now()
	tok := signJWT(t, "s", map[string]any{
		"userId":        int64(54321),
		"domainAccount": "test.user",
		"iat":           now.Add(-36 * time.Minute).Unix(),
		"exp":           now.Add(15 * 24 * time.Hour).Unix(),
	})
	w := postBearerExchange(o, `{"access_token":"`+tok+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s — a token issued 36 minutes ago is the normal "+
			"shape of a client that did not exchange the instant it logged in", w.Code, w.Body.String())
	}
}

// 台账拒绝与验签失败,**响应内容**必须一模一样:区分两者等于告诉调用方"这张
// token 曾经是有效的"。
//
// 只钉响应,不钉时序:验签失败在碰 Redis 之前就返回了,而验签通过但被台账拒的
// 那条要多走一次 Redis 往返,两者理论上可由重复测量区分开。没有去抹平它 ——
// 代价是给每个坏签名都加一次 Redis 调用,而能利用这个信道的人本来就已经握着
// 这张 token 了。这里如实写下来,免得用例名被读成"时序也一致"。
func TestExchangeJWT_LedgerRefusalReturnsTheSameResponseAsABadToken(t *testing.T) {
	issuer := "https://idp-test.example.com#bearer-jwt"
	now := time.Now()
	good := signJWT(t, "s", map[string]any{
		"userId": int64(1), "domainAccount": "u",
		"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Hour).Unix(),
	})

	refused := newTestOIDCForBearerJWT(t, []byte("s"), issuer, &fakeUserLookup{}, newFakeIdentityStore())
	refused.redeemLedger = &fakeRedemptionLedger{outcome: redeemRejectIdle}
	wRefused := postBearerExchange(refused, `{"access_token":"`+good+`"}`)

	badSig := newTestOIDCForBearerJWT(t, []byte("s"), issuer, &fakeUserLookup{}, newFakeIdentityStore())
	badSig.redeemLedger = &fakeRedemptionLedger{outcome: redeemAdmitFirst}
	wBadSig := postBearerExchange(badSig, `{"access_token":"`+signJWT(t, "other-secret", map[string]any{
		"userId": int64(1), "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})+`"}`)

	if wRefused.Code != wBadSig.Code {
		t.Errorf("status: ledger refusal %d, bad signature %d — they must match",
			wRefused.Code, wBadSig.Code)
	}
	if wRefused.Body.String() != wBadSig.Body.String() {
		t.Errorf("body differs:\n ledger refusal: %s\n bad signature:  %s",
			wRefused.Body.String(), wBadSig.Body.String())
	}
}

// 台账拒绝时不能建号、不能发会话。
func TestExchangeJWT_LedgerRefusalIssuesNothing(t *testing.T) {
	users := &fakeUserLookup{
		loginResp: &IssueSessionResp{UID: "uid-x", LoginRespJSON: `{"token":"sess-x"}`},
	}
	store := newFakeIdentityStore()
	o := newTestOIDCForBearerJWT(t, []byte("s"), "https://idp-test.example.com#bearer-jwt", users, store)
	o.redeemLedger = &fakeRedemptionLedger{outcome: redeemRejectStaleFirst}

	now := time.Now()
	tok := signJWT(t, "s", map[string]any{
		"userId": int64(7), "domainAccount": "u",
		"iat": now.Add(-30 * 24 * time.Hour).Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	w := postBearerExchange(o, `{"access_token":"`+tok+`"}`)
	if w.Code == http.StatusOK {
		t.Fatalf("a refused redemption returned 200: %s", w.Body.String())
	}
	if len(store.written) != 0 {
		t.Errorf("identity rows written = %d, want 0: a refused redemption must not create "+
			"an account", len(store.written))
	}
	if strings.Contains(w.Body.String(), "sess-x") {
		t.Error("a session token leaked into a refused response")
	}
}

// ---- Redis 实现 --------------------------------------------------------------

// 判定表跑在真 Redis 上(Lua 脚本的语义只能在这里验)。
func TestRedisRedemptionLedger_DecisionTable_Integration(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	p := redemptionPolicy{firstRedeemMaxAge: time.Hour, idleWindow: 2 * time.Hour}
	l := newRedisRedemptionLedger(ctx, p)
	t.Cleanup(func() {
		if err := l.Close(); err != nil {
			t.Logf("close: %v", err)
		}
	})

	now := time.Now()
	exp := now.Add(15 * 24 * time.Hour)

	// 每个子用例一个独立摘要(带纳秒盐),避免同 binary 重跑时串扰。
	//
	// 盐意味着**清理不能靠"跑之前先删"** —— 那删的永远是一个不存在的 key。落账
	// 的记录 TTL 是 token 的剩余寿命(这里 15 天),不清理就会在共享测试 Redis 里
	// 堆两周,而 CleanAllTables 不碰 Redis。所以登记用过的 key,退出时删。
	var used []string
	digest := func(name string) string {
		d := redemptionDigest(t.Name() + name + now.String())
		used = append(used, redemptionKey(d))
		return d
	}
	t.Cleanup(func() {
		if len(used) == 0 {
			return
		}
		if err := l.client.Del(used...).Err(); err != nil {
			t.Logf("cleanup %d ledger keys: %v", len(used), err)
		}
	})

	t.Run("first redemption inside F is admitted", func(t *testing.T) {
		d := digest("first")
		got, err := l.Admit(context.Background(), d, now.Add(-30*time.Minute), exp, now)
		if err != nil {
			t.Fatalf("admit: %v", err)
		}
		if got != redeemAdmitFirst {
			t.Fatalf("got %s, want %s", got, redeemAdmitFirst)
		}
		// 记录必须活到 token 的 exp,不是活到 T。
		ttl, err := l.client.TTL(redemptionKey(d)).Result()
		if err != nil {
			t.Fatalf("ttl: %v", err)
		}
		if ttl <= p.idleWindow {
			t.Errorf("record ttl = %v, want the token's remaining life; a record that dies "+
				"before the token makes a repeat redemption look like a first one", ttl)
		}
	})

	t.Run("first redemption beyond F is refused and leaves no record", func(t *testing.T) {
		d := digest("stale-first")
		got, err := l.Admit(context.Background(), d, now.Add(-10*time.Hour), exp, now)
		if err != nil {
			t.Fatalf("admit: %v", err)
		}
		if got != redeemRejectStaleFirst {
			t.Fatalf("got %s, want %s", got, redeemRejectStaleFirst)
		}
		if n, err := l.client.Exists(redemptionKey(d)).Result(); err != nil || n != 0 {
			t.Errorf("a refused redemption wrote a record (exists=%d, err=%v); it would "+
				"open the idle window for a token that was never admitted", n, err)
		}
	})

	t.Run("repeat redemption inside T is admitted and refreshes last_at", func(t *testing.T) {
		d := digest("repeat")
		iat := now.Add(-30 * time.Minute)
		if _, err := l.Admit(context.Background(), d, iat, exp, now); err != nil {
			t.Fatalf("first admit: %v", err)
		}
		later := now.Add(90 * time.Minute) // 距上次 90m < T=2h
		got, err := l.Admit(context.Background(), d, iat, exp, later)
		if err != nil {
			t.Fatalf("second admit: %v", err)
		}
		if got != redeemAdmitRepeat {
			t.Fatalf("got %s, want %s; a client that keeps using its token must never be "+
				"refused, whatever its iat", got, redeemAdmitRepeat)
		}
		// 再隔 90 分钟:距**上次兑换**仍在 T 内,尽管距 iat 已远超 F。
		evenLater := later.Add(90 * time.Minute)
		got, err = l.Admit(context.Background(), d, iat, exp, evenLater)
		if err != nil {
			t.Fatalf("third admit: %v", err)
		}
		if got != redeemAdmitRepeat {
			t.Fatalf("got %s, want %s: the window slides on each redemption", got, redeemAdmitRepeat)
		}
	})

	t.Run("repeat redemption beyond T is refused", func(t *testing.T) {
		d := digest("idle")
		iat := now.Add(-10 * time.Minute)
		if _, err := l.Admit(context.Background(), d, iat, exp, now); err != nil {
			t.Fatalf("first admit: %v", err)
		}
		abandoned := now.Add(3 * time.Hour) // > T=2h
		got, err := l.Admit(context.Background(), d, iat, exp, abandoned)
		if err != nil {
			t.Fatalf("admit: %v", err)
		}
		if got != redeemRejectIdle {
			t.Fatalf("got %s, want %s; this is the case the ledger exists for: a token whose "+
				"owner stopped using it, redeemed by whoever captured it", got, redeemRejectIdle)
		}
	})

	// 判定与落账必须在一次往返里完成。两步 GET+SET 的话,并发的首次兑换会各自读到
	// "无记录"再各自写回,于是同一张 token 被当成多次首次兑换 —— 窗口判定失去意义。
	t.Run("concurrent first redemptions produce exactly one admit_first", func(t *testing.T) {
		d := digest("race")
		const n = 8
		res := make(chan redemptionOutcome, n)
		for i := 0; i < n; i++ {
			go func() {
				out, err := l.Admit(context.Background(), d, now.Add(-time.Minute), exp, now)
				if err != nil {
					t.Errorf("admit: %v", err)
				}
				res <- out
			}()
		}
		firsts := 0
		for i := 0; i < n; i++ {
			if <-res == redeemAdmitFirst {
				firsts++
			}
		}
		if firsts != 1 {
			t.Errorf("%d concurrent redemptions produced %d admit_first, want exactly 1: "+
				"the decision and the write must be one atomic round trip", n, firsts)
		}
	})

	// F3 回归:亚秒配置经 normalized 收敛后,脚本拿到的是 >=1 的整秒。未收敛时
	// ARGV 是 "0",脚本比的是 `now - iat > 0`,任何一秒前签发的 token 都被拒。
	t.Run("a sub-second bound does not reject everything", func(t *testing.T) {
		sub := &redisRedemptionLedger{client: l.client, policy: redemptionPolicy{
			firstRedeemMaxAge: 500 * time.Millisecond,
			idleWindow:        300 * time.Millisecond,
		}.normalized()}
		got, err := sub.Admit(context.Background(), digest("subsecond"),
			now.Add(-100*time.Millisecond), exp, now)
		if err != nil {
			t.Fatalf("admit: %v", err)
		}
		if got != redeemAdmitFirst {
			t.Errorf("got %s, want %s", got, redeemAdmitFirst)
		}
	})

	t.Run("a corrupt record is treated as absent", func(t *testing.T) {
		d := digest("corrupt")
		if err := l.client.Set(redemptionKey(d), "not-a-timestamp", time.Hour).Err(); err != nil {
			t.Fatalf("seed: %v", err)
		}
		got, err := l.Admit(context.Background(), d, now.Add(-5*time.Minute), exp, now)
		if err != nil {
			t.Fatalf("admit: %v", err)
		}
		if got != redeemAdmitFirst {
			t.Errorf("got %s, want %s: a corrupt value is equivalent to a deleted key, and "+
				"whoever can write one can also delete it", got, redeemAdmitFirst)
		}
	})
}

// Close 与在途请求可能并发。它只关连接池,不改 handler 在请求路径上读的接口字段
// —— 后者是一次无同步的接口值写,轻则静默走降级,重则读到半个接口值。
func TestOIDCClose_DoesNotMutateTheFieldTheRequestPathReads(t *testing.T) {
	led := &fakeRedemptionLedger{outcome: redeemAdmitFirst}
	o := &OIDC{Log: log.NewTLog("OIDC-test"), redeemLedger: led}
	if err := o.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if o.redeemLedger != redemptionLedger(led) {
		t.Error("Close() replaced redeemLedger; that field is read by in-flight handlers")
	}
	// 幂等:再关一次不应报错(New() 的失败路径会调一次,框架退出再调一次)。
	if err := o.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}
