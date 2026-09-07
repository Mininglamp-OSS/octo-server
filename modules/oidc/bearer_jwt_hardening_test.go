package oidc

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// bearer JWT 的信任锚强度。
//
// 这个端点的全部安全性建立在一把对称密钥上:持有它就能为**任意** userId 签一张
// 能换取会话的 token。所以密钥强度和 token 新鲜度不是调优项,是准入条件。
// -----------------------------------------------------------------------------

// P1-4(1):任何非空密钥都被接受 → 一字节或字典词也是"生产合法"。
//
// 同模块已有正确的先例:DM_OIDC_RT_ENC_KEY 必须 base64 解出恰好 32 字节,
// 否则 LoadConfig 直接拒绝启动(config.go)。HMAC 密钥的强度要求不比它低 ——
// 短密钥可以被离线爆破:攻击者只要拿到**一张**合法 JWT 就能本地穷举出密钥,
// 之后可以伪造任意用户的登录。
func TestBearerJWTSecret_TooShortIsRefused(t *testing.T) {
	cases := map[string]struct {
		secret  string
		wantErr bool
	}{
		"empty":                {"", true},
		"one byte":             {"x", true},
		"dictionary word":      {"password", true},
		"31 bytes":             {strings.Repeat("a", 31), true},
		"32 bytes":             {strings.Repeat("a", 32), false},
		"longer than required": {strings.Repeat("a", 64), false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := validateBearerJWTSecret([]byte(c.secret))
			if c.wantErr && err == nil {
				t.Fatalf("secret of %d bytes was accepted; it must be refused", len(c.secret))
			}
			if !c.wantErr && err != nil {
				t.Fatalf("secret of %d bytes was refused: %v", len(c.secret), err)
			}
		})
	}
}

// 拒绝信息不能回显密钥,只能给长度。
func TestBearerJWTSecret_ErrorDoesNotLeakSecret(t *testing.T) {
	const secret = "short-but-memorable"
	err := validateBearerJWTSecret([]byte(secret))
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error %q contains the secret itself", err.Error())
	}
}

// P1-4(3):exp 是唯一的新鲜度控制 → 抓到一张 assertion 就能在 exp 之前反复
// 兑换新会话,**包括用户已经登出之后**。
//
// 上游那张 JWT 的 exp 是签发后约 15 天,而它的用途是"登录那一刻换一次会话"。
// 15 天的可重放窗口与用途完全不匹配,而我们无法验证上游的吊销状态(黑名单在
// 对方 Redis 里,我方连不上;而且据现状那条黑名单写入本身是坏的)。
//
// 我方因此需要一道自己的新鲜度判定,不需要上游改任何东西。
//
// **本用例验的是 verifyBearerJWT 这个纯函数的 maxAge 参数**,不是生产策略:生产
// 上兑换的新鲜度已改由兑换台账按兑换行为判定(redemption_ledger.go 的 F/T),
// 两个生产调用方都给 maxAge=0。参数与哨兵留着,是为了将来要给某条路径重新加
// iat 上限时不必再实现一遍;它一旦被接回某条路径,这里就是它的行为基线。
func TestVerifyBearerJWT_RejectsTokenOlderThanMaxLifetime(t *testing.T) {
	secret := strings.Repeat("k", 32)
	now := time.Now()

	fresh := signJWT(t, secret, map[string]any{
		"userId": 1,
		"iat":    now.Add(-1 * time.Minute).Unix(),
		"exp":    now.Add(15 * 24 * time.Hour).Unix(),
	})
	stale := signJWT(t, secret, map[string]any{
		"userId": 1,
		// 10 天前签发,但 exp 还远 —— 正是"抓包后长期复用"的形态。
		"iat": now.Add(-10 * 24 * time.Hour).Unix(),
		"exp": now.Add(5 * 24 * time.Hour).Unix(),
	})

	if _, err := verifyBearerJWT(fresh, []byte(secret), now, testBearerJWTMaxAge); err != nil {
		t.Fatalf("a freshly issued token must be accepted: %v", err)
	}
	_, err := verifyBearerJWT(stale, []byte(secret), now, testBearerJWTMaxAge)
	if err == nil {
		t.Fatal("a token issued 10 days ago was accepted; exp alone lets a captured " +
			"assertion mint fresh sessions for its whole lifetime, including after logout")
	}
	if !errors.Is(err, ErrJWTTooOld) {
		t.Errorf("err = %v, want ErrJWTTooOld", err)
	}
}

// 缺 iat 的 token 无法判断新鲜度 —— 必须拒绝,不能当作"很新"放行。
//
// fail-open 在这里等于把上限当装饰:攻击者只要把 iat 去掉就绕过了。
func TestVerifyBearerJWT_MissingIatIsRefused(t *testing.T) {
	secret := strings.Repeat("k", 32)
	now := time.Now()
	noIat := signJWT(t, secret, map[string]any{
		"userId": 1,
		"exp":    now.Add(time.Hour).Unix(),
	})
	if _, err := verifyBearerJWT(noIat, []byte(secret), now, testBearerJWTMaxAge); err == nil {
		t.Fatal("a token without iat was accepted; freshness cannot be checked without " +
			"it, so dropping iat would bypass the max-lifetime ceiling")
	}
}

// 时钟偏移容忍:iat 略微在未来(几秒到一分钟)是签发方与我方时钟不同步的常态,
// 不该当成攻击。但**远期** iat 要拒 —— 那是把 token 的可用窗口往后推。
func TestVerifyBearerJWT_TolerateSmallClockSkewButRejectFarFutureIat(t *testing.T) {
	secret := strings.Repeat("k", 32)
	now := time.Now()

	skewed := signJWT(t, secret, map[string]any{
		"userId": 1,
		"iat":    now.Add(30 * time.Second).Unix(), // 我方时钟慢了半分钟
		"exp":    now.Add(time.Hour).Unix(),
	})
	if _, err := verifyBearerJWT(skewed, []byte(secret), now, testBearerJWTMaxAge); err != nil {
		t.Errorf("a 30s clock skew must be tolerated, got %v", err)
	}

	farFuture := signJWT(t, secret, map[string]any{
		"userId": 1,
		"iat":    now.Add(24 * time.Hour).Unix(),
		"exp":    now.Add(48 * time.Hour).Unix(),
	})
	if _, err := verifyBearerJWT(farFuture, []byte(secret), now, testBearerJWTMaxAge); err == nil {
		t.Error("an iat a day in the future was accepted; that shifts the token's usable " +
			"window forward and defeats the ceiling")
	}
}

// 验签器不再按用途区分新鲜度 —— 两种用途都只验签名与 exp,兑换那条路的新鲜度
// 由兑换台账按**兑换行为**判定(redemption_ledger.go)。
//
// 曾经这里是"兑换套 10 分钟 iat 上限、认证用 exp"。那个上限的锚点是 iat,即上游
// 签发的时刻,与"用户什么时候真的来兑换"无关:登录后隔半小时才兑换的合法客户端
// 被拒(返回的 401 与"凭据无效"不可区分),而窗口内抓到 token 的攻击者照样能兑。
//
// 这个测试现在钉的是**验签器一侧的不变量**:两种用途对同一张 token 给出相同答案,
// 且 exp 在两边都仍然强制。兑换准入的两个边界由 redemption_ledger_test.go 覆盖。
func TestBearerJWTVerifier_VerificationIsFreshnessAgnostic(t *testing.T) {
	secret := strings.Repeat("k", 32)
	v := newBearerJWTVerifierForTest([]byte(secret), "https://idp.example.com#bearer-jwt")
	now := time.Now()

	// 10 天前签发,exp 还有 5 天 —— 桌面端正常复用的形态。
	longLived := signJWT(t, secret, map[string]any{
		"userId":        1,
		"domainAccount": "desk.user",
		"iat":           now.Add(-10 * 24 * time.Hour).Unix(),
		"exp":           now.Add(5 * 24 * time.Hour).Unix(),
	})

	rj, err := v.VerifyForRedemption(longLived, now)
	if err != nil {
		t.Fatalf("verification must not judge freshness: a 10-day-old assertion still "+
			"verifies; whether it may mint a session is the ledger's call: %v", err)
	}
	// 台账需要这两个时间戳,而它们必须来自这一次解析 —— 让调用方自己再解一遍
	// payload,两处对同一张 token 得出不同的 iat,判定就无从谈起。
	if got := rj.IssuedAt.Unix(); got != now.Add(-10*24*time.Hour).Unix() {
		t.Errorf("IssuedAt = %d, want the token's own iat", got)
	}
	if got := rj.ExpiresAt.Unix(); got != now.Add(5*24*time.Hour).Unix() {
		t.Errorf("ExpiresAt = %d, want the token's own exp", got)
	}
	if rj.Claims == nil || rj.Claims.Subject != "1" {
		t.Errorf("Claims = %+v, want the mapped identity", rj.Claims)
	}

	if _, err := v.VerifyForAuthentication(longLived, now); err != nil {
		t.Errorf("authentication must accept a token still within its exp, got %v; "+
			"refusing it breaks the desktop client ten minutes after login", err)
	}

	// 但真正过了 exp,两种用途都必须拒 —— 去掉 iat 上限不等于去掉过期校验。
	expired := signJWT(t, secret, map[string]any{
		"userId": 1,
		"iat":    now.Add(-20 * 24 * time.Hour).Unix(),
		"exp":    now.Add(-5 * 24 * time.Hour).Unix(),
	})
	if _, err := v.VerifyForAuthentication(expired, now); err == nil {
		t.Error("authentication must still honour exp — dropping the ceiling is not the " +
			"same as dropping expiry")
	}
	if _, err := v.VerifyForRedemption(expired, now); err == nil {
		t.Error("redemption must refuse an expired token")
	}
}
