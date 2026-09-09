package oidc

// bearer_jwt_test.go — 客户端自签 bearer JWT (HS256) 的解析与验签测试。
//
// 为什么不引 golang-jwt 库:这个 token 永远是 HS256,永远只含我们已知的 5 个
// 字段;自己写能强制拒绝 alg:none / alg=RS256 等混淆攻击,代码量 <100 行,
// 不增加依赖,审计面小。库引入会带来"帮你处理所有 alg"的默认行为,反而扩大
// 攻击面(经典 JWT 库漏洞:没显式限制 alg 时 RS256 公钥被当 HMAC key 用)。

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// 用测试密钥签一个合法 token,避免测试依赖真实密钥 / 真实 token 样本。
// 注意:此函数仅供测试构造 fixture 使用,生产路径不要调用。
func signBearerToken(t *testing.T, secret string, claims map[string]interface{}) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	b, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(b)
	sig := hmac.New(sha256.New, []byte(secret))
	sig.Write([]byte(header + "." + payload))
	signature := base64.RawURLEncoding.EncodeToString(sig.Sum(nil))
	return header + "." + payload + "." + signature
}

// 测试使用的合成 HS256 密钥(32 字节),专门为单测生成,非任何真实环境凭据。
const bearerJWTTestSecret = "test-jwt-secret-not-real-12345678"

// testBearerJWTMaxAge 只给这些用例用,验的是 verifyBearerJWT 这个**纯函数**的
// maxAge 参数本身。
//
// 生产路径两个调用方现在都传 0:兑换的新鲜度由兑换台账按兑换行为判定
// (redemption_ledger.go 的 F/T),常驻认证器用 token 自己的 exp。这个常量因此
// 留在测试里而不是生产代码里 —— 一个生产不用的策略常量放在 bearer_jwt.go 中,
// 迟早会有人把它接回某条路径,把 iat 锚点的老问题重新装上。
const testBearerJWTMaxAge = 10 * time.Minute

// -- RED 阶段:先写测试,再写 verifyBearerJWT / 解析逻辑。 --------------------

func TestBearerJWT_HappyPath(t *testing.T) {
	now := time.Now()
	const synthUserID = int64(54321)
	const synthName = "test.user"
	tok := signBearerToken(t, bearerJWTTestSecret, map[string]interface{}{
		"userId":        float64(synthUserID), // JSON 数字在 decode 后是 float64
		"domainAccount": synthName,
		"iat":           float64(now.Add(-1 * time.Minute).Unix()),
		"exp":           float64(now.Add(15 * 24 * time.Hour).Unix()),
	})
	claims, err := verifyBearerJWT(tok, []byte(bearerJWTTestSecret), now, testBearerJWTMaxAge)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.UserID != synthUserID {
		t.Errorf("userId=%d, want %d", claims.UserID, synthUserID)
	}
	if claims.DomainAccount != synthName {
		t.Errorf("domainAccount=%q, want %q", claims.DomainAccount, synthName)
	}
}

func TestBearerJWT_WrongSecret(t *testing.T) {
	tok := signBearerToken(t, bearerJWTTestSecret, map[string]interface{}{
		"userId": float64(1), "exp": float64(time.Now().Add(time.Hour).Unix()),
	})
	_, err := verifyBearerJWT(tok, []byte("some-other-secret"), time.Now(), testBearerJWTMaxAge)
	if err == nil {
		t.Fatal("expected error for wrong secret, got nil")
	}
	if !errors.Is(err, ErrJWTInvalidSig) {
		t.Errorf("err = %v, want ErrJWTInvalidSig", err)
	}
}

func TestBearerJWT_Expired(t *testing.T) {
	now := time.Now()
	tok := signBearerToken(t, bearerJWTTestSecret, map[string]interface{}{
		"userId": float64(1),
		"iat":    float64(now.Add(-2 * time.Hour).Unix()),
		"exp":    float64(now.Add(-1 * time.Minute).Unix()),
	})
	_, err := verifyBearerJWT(tok, []byte(bearerJWTTestSecret), now, testBearerJWTMaxAge)
	if err == nil {
		t.Fatal("expected error for expired token, got nil")
	}
	if !errors.Is(err, ErrJWTExpired) {
		t.Errorf("err = %v, want ErrJWTExpired", err)
	}
}

func TestBearerJWT_Malformed(t *testing.T) {
	cases := map[string]string{
		"empty":        "",
		"two parts":    "a.b",
		"four parts":   "a.b.c.d",
		"bad base64":   "!!!.!!!.!!!",
		"bad json hdr": signBearerToken(t, bearerJWTTestSecret, map[string]interface{}{"userId": 1})[:20] + "..." + signBearerToken(t, bearerJWTTestSecret, map[string]interface{}{"userId": 1})[20:], // 重拼,无意义
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := verifyBearerJWT(tok, []byte(bearerJWTTestSecret), time.Now(), testBearerJWTMaxAge)
			if err == nil {
				t.Fatalf("%s: expected error", name)
			}
		})
	}
}

// alg 混淆攻击:客户端发送 alg=none 或 alg=RS256 的 token,试图绕过签名校验。
// 这是 JWT 生态最经典的漏洞,必须明确拒绝。
func TestBearerJWT_AlgMustBeHS256(t *testing.T) {
	// alg=none:无签名
	noneHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"userId":1,"exp":4000000000}`))
	noneToken := noneHeader + "." + payload + "."
	if _, err := verifyBearerJWT(noneToken, []byte(bearerJWTTestSecret), time.Now(), testBearerJWTMaxAge); err == nil {
		t.Fatal("alg=none must be rejected")
	}

	// 构造 alg=RS256 的 header(内容合法但不是 HS256),签名是假的
	rsHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	rsTok := rsHeader + "." + payload + ".fakesig"
	if _, err := verifyBearerJWT(rsTok, []byte(bearerJWTTestSecret), time.Now(), testBearerJWTMaxAge); err == nil {
		t.Fatal("alg=RS256 must be rejected (we only accept HS256)")
	}
}

func TestBearerJWT_MissingUserID(t *testing.T) {
	tok := signBearerToken(t, bearerJWTTestSecret, map[string]interface{}{
		"exp": float64(time.Now().Add(time.Hour).Unix()),
		// 没有 userId
	})
	_, err := verifyBearerJWT(tok, []byte(bearerJWTTestSecret), time.Now(), testBearerJWTMaxAge)
	if err == nil {
		t.Fatal("expected error for missing userId")
	}
}

func TestBearerJWT_ZeroUserIDRejected(t *testing.T) {
	// userId=0 是异常值,可能表示未认证/默认,不能被接受。
	tok := signBearerToken(t, bearerJWTTestSecret, map[string]interface{}{
		"userId": float64(0), "exp": float64(time.Now().Add(time.Hour).Unix()),
	})
	_, err := verifyBearerJWT(tok, []byte(bearerJWTTestSecret), time.Now(), testBearerJWTMaxAge)
	if err == nil {
		t.Fatal("userId=0 must be rejected (anonymous / not-logged-in sentinel)")
	}
}

// toIdentityClaims 把 bearerJWTClaims 转成我们的 IdentityClaims,issuer 由
// 调用方注入。subject 用 strconv.FormatInt(userId,10)。
func TestBearerJWT_ToIdentityClaims(t *testing.T) {
	now := time.Now()
	const synthUserID = int64(54321)
	const synthName = "test.user"
	tok := signBearerToken(t, bearerJWTTestSecret, map[string]interface{}{
		"userId":        float64(synthUserID),
		"domainAccount": synthName,
		"iat":           float64(now.Add(-1 * time.Minute).Unix()),
		"exp":           float64(now.Add(time.Hour).Unix()),
	})
	claims, err := verifyBearerJWT(tok, []byte(bearerJWTTestSecret), now, testBearerJWTMaxAge)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	ic := claims.toIdentityClaims("https://idp-test.example.com#bearer-jwt")
	if ic.Issuer != "https://idp-test.example.com#bearer-jwt" {
		t.Errorf("issuer=%q, want https://idp-test.example.com#bearer-jwt", ic.Issuer)
	}
	wantSubject := "54321"
	if ic.Subject != wantSubject {
		t.Errorf("subject=%q, want %q", ic.Subject, wantSubject)
	}
	if ic.Name != synthName {
		t.Errorf("name=%q, want %q (used as display name fallback)", ic.Name, synthName)
	}
	// bearer JWT 不携带 email/phone/verified 位,必须留零值(fail-closed:false),
	// 避免被 AutoLink 误认成已验证。
	if ic.Email != "" || ic.EmailVerified || ic.PhoneVerified {
		t.Errorf("email/verified flags must be zero: email=%q ev=%v pv=%v", ic.Email, ic.EmailVerified, ic.PhoneVerified)
	}
}

// -- issuer 派生 --------------------------------------------------------------
//
// 这个函数决定 bearer JWT 路径的身份命名空间,值一旦上线不可更改(改了等于该
// 路径全员重建号),所以每条约束都要有守卫。

func TestBearerJWTIssuerFromUpstream_DerivesDistinctNamespace(t *testing.T) {
	const upstream = "https://idp.example.com"
	got, err := bearerJWTIssuerFromUpstream(upstream)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if got == upstream {
		t.Fatal("derived issuer must differ from the upstream issuer, otherwise the two " +
			"ID spaces collide on (issuer, subject)")
	}
	if want := upstream + bearerJWTIssuerSuffix; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// 环境隔离不再靠单独的环境标识 env,而是继承上游 issuer 的每环境取值。
// 这条测试就是在断言那个继承关系成立。
func TestBearerJWTIssuerFromUpstream_InheritsEnvironmentIsolation(t *testing.T) {
	test, err := bearerJWTIssuerFromUpstream("https://idp-test.example.com")
	if err != nil {
		t.Fatalf("derive test: %v", err)
	}
	prod, err := bearerJWTIssuerFromUpstream("https://idp.example.com")
	if err != nil {
		t.Fatalf("derive prod: %v", err)
	}
	if test == prod {
		t.Fatal("test and prod must derive to different issuers, else test identity rows " +
			"would be indistinguishable from production ones")
	}
}

func TestBearerJWTIssuerFromUpstream_EmptyRejected(t *testing.T) {
	for _, in := range []string{"", "   ", "\t"} {
		if _, err := bearerJWTIssuerFromUpstream(in); err == nil {
			t.Errorf("upstream issuer %q must be rejected", in)
		}
	}
}

// 防止把已派生的值再喂回来(例如运维把日志里看到的 issuer 复制进 env),
// 那会得到一个双后缀的第三个命名空间,静默把已绑的用户变成"查不到"。
func TestBearerJWTIssuerFromUpstream_AlreadyDerivedRejected(t *testing.T) {
	derived, err := bearerJWTIssuerFromUpstream("https://idp.example.com")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if _, err := bearerJWTIssuerFromUpstream(derived); err == nil {
		t.Fatal("an already-derived issuer must be rejected, not double-suffixed")
	}
}

// 列宽守卫:超长必须启动期拒绝。MySQL 在非严格 sql_mode 下会静默截断,
// 而两个不同 issuer 截成同一个值会让 uk_issuer_subject 把两个人合成一个号。
func TestBearerJWTIssuerFromUpstream_TooLongRejected(t *testing.T) {
	fits := "https://" + strings.Repeat("a", issuerMaxLen-len("https://")-len(bearerJWTIssuerSuffix))
	got, err := bearerJWTIssuerFromUpstream(fits)
	if err != nil {
		t.Fatalf("issuer that exactly fills the column must be accepted: %v", err)
	}
	if len(got) != issuerMaxLen {
		t.Fatalf("boundary fixture is wrong: derived length %d, want %d", len(got), issuerMaxLen)
	}
	if _, err := bearerJWTIssuerFromUpstream(fits + "a"); err == nil {
		t.Fatalf("issuer exceeding %d bytes must be rejected", issuerMaxLen)
	}
}
