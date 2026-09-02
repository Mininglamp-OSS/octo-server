package oidc

// foreign_token_classification_test.go — "这张 token 是不是我们签的" 的判定边界。
//
// 这个判定是安全相关的:判成"不是我们的"意味着凭据会被回落转发到上游 IdP,
// 而上游那条路径把凭据放在 **URL query** 上(oauth2_provider.go 的 /userinfo)。
// 于是误判一次 = 一份在我方密钥下合法的签名 + 载荷里的 PII 落进第三方访问日志。
//
// 原先的判定按错误哨兵的身份做:ErrJWTMalformed / ErrJWTBadAlg / ErrJWTInvalidSig
// 三者视为"不是我们的"。**这个前提是假的** —— ErrJWTMalformed 横跨 hmac.Equal
// 两侧:
//
//	段数 != 3 / header base64 / header json / payload base64 / signature base64
//	    ↑ 验签前,确实无法断定归属
//	hmac.Equal ────────────────────────────────────────────── 分界线
//	payload json / exp 不是整数 / payload decode to out
//	    ↓ 验签**已通过**,这张 token 确定是我们签的,却同样报 ErrJWTMalformed
//
// 所以判定不能从错误身份推断,必须由**产生错误的位置**显式标注。方向也必须反过来:
// 只给验签前/验签本身的失败打 ErrJWTForeign,判定只认这个标记。这样将来在
// VerifyHS256JWT 里新增任何检查,默认都落进"是我们的"(不转发)—— fail-closed。
// -----------------------------------------------------------------------------

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const classificationSecret = "classification-test-secret-32byte!"

// signHS256Raw 用给定密钥签一段**原样给定**的 header/payload JSON。
//
// 不能用 signBearerToken / signDesktopJWT:那两个走 json.Marshal,只能造出类型
// 正确的 payload,而本文件要证明的恰好是"类型写错但签名合法"这一类 —— 用现有
// helper 表达不出来,这正是这一类当初漏掉的原因。
func signHS256Raw(t *testing.T, secret, headerJSON, payloadJSON string) string {
	t.Helper()
	h := base64.RawURLEncoding.EncodeToString([]byte(headerJSON))
	p := base64.RawURLEncoding.EncodeToString([]byte(payloadJSON))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(h + "." + p))
	return h + "." + p + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// 一张带着我方合法 HMAC、但 payload 字段类型写错的 token,必须被认成"我们的"。
//
// 每一条都不是刻意构造的畸形输入,而是别的语言的后端很容易写出来的形态:
// JavaScript 的 `iat: Date.now()/1000` 不取整就是浮点;把整数 id 序列化成字符串
// 是 JSON API 里最常见的一种做法。而客户端会把这张 token 存下来在整个有效期内
// 反复出示,所以泄漏是**持续的**,不是一次性的。
func TestIsForeignToken_ValidHMACWithWrongPayloadTypesIsOurs(t *testing.T) {
	const hdr = `{"alg":"HS256","typ":"JWT"}`
	exp := time.Now().Add(24 * time.Hour).Unix()
	iat := time.Now().Add(-time.Minute).Unix()

	cases := map[string]string{
		// userId 序列化成字符串 —— 失败点在 payload decode to out(验签后)。
		"userId as string": fmt.Sprintf(
			`{"userId":"2200005","domainAccount":"desk.user","iat":%d,"exp":%d}`, iat, exp),
		// iat 是浮点 —— Date.now()/1000 未取整。失败点同上。
		"iat as float": fmt.Sprintf(
			`{"userId":2200005,"domainAccount":"desk.user","iat":%d.75,"exp":%d}`, iat, exp),
		// exp 是字符串 —— 失败点在 "exp is not an integer"(验签后)。
		"exp as string": fmt.Sprintf(
			`{"userId":2200005,"domainAccount":"desk.user","iat":%d,"exp":"%d"}`, iat, exp),
		// payload 是合法 base64,但不是合法 JSON —— 失败点在 payload json(验签后)。
		"payload is not json": `this-is-not-json`,
	}

	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			tok := signHS256Raw(t, classificationSecret, hdr, payload)

			var c bearerJWTClaims
			err := VerifyHS256JWT(tok, []byte(classificationSecret), time.Now(), &c)
			if err == nil {
				t.Fatalf("expected the token to be rejected; the case is only meaningful if it fails")
			}
			if IsForeignToken(err) {
				t.Fatalf("IsForeignToken(%v) = true, want false: this token carries a valid HMAC "+
					"under our own secret, so it is unambiguously ours. Classifying it foreign makes "+
					"the caller forward it to the upstream IdP, which takes credentials in the URL "+
					"query — leaking the payload PII and a signature that is valid under the secret "+
					"whose 32-byte floor exists precisely to stop offline recovery", err)
			}
		})
	}
}

// 反面:验签前/验签本身失败的必须仍然判成"不是我们的",否则上游凭据路径被掐断。
func TestIsForeignToken_PreSignatureFailuresStayForeign(t *testing.T) {
	const hdr = `{"alg":"HS256","typ":"JWT"}`
	payload := fmt.Sprintf(`{"userId":2200005,"iat":%d,"exp":%d}`,
		time.Now().Add(-time.Minute).Unix(), time.Now().Add(24*time.Hour).Unix())
	good := signHS256Raw(t, classificationSecret, hdr, payload)
	parts := strings.Split(good, ".")

	cases := map[string]string{
		"two segments":          parts[0] + "." + parts[1],
		"four segments":         good + ".extra",
		"opaque upstream token": "0b2f3c4d5e6f7a8b9c0d1e2f",
		"header not base64":     "!!!" + "." + parts[1] + "." + parts[2],
		"payload not base64":    parts[0] + "." + "!!!" + "." + parts[2],
		"signature not base64":  parts[0] + "." + parts[1] + "." + "!!!",
		"header not json": base64.RawURLEncoding.EncodeToString([]byte("not-json")) +
			"." + parts[1] + "." + parts[2],
		"alg is RS256":   signHS256Raw(t, classificationSecret, `{"alg":"RS256","typ":"JWT"}`, payload),
		"typ is not JWT": signHS256Raw(t, classificationSecret, `{"alg":"HS256","typ":"MAC"}`, payload),
		"signed with another secret": signHS256Raw(t,
			"a-completely-different-secret-32b!", hdr, payload),
	}

	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			var c bearerJWTClaims
			err := VerifyHS256JWT(tok, []byte(classificationSecret), time.Now(), &c)
			if err == nil {
				t.Fatalf("expected rejection")
			}
			if !IsForeignToken(err) {
				t.Fatalf("IsForeignToken(%v) = false, want true: this failure happens at or before "+
					"hmac.Equal, so we cannot claim the token is ours. Treating it as ours would "+
					"cut off the upstream credential path entirely", err)
			}
		})
	}
}

// 判定必须是**白名单**:只认显式的 ErrJWTForeign 标记。
//
// 这条钉住的是失败方向。旧实现是黑名单(列出三个"不是我们的"哨兵),于是任何
// 将来新增的错误都默认落进"不是我们的" = 默认转发上游 = fail-open。反过来之后,
// 新增检查默认落进"是我们的" = 默认不转发 = fail-closed。
func TestIsForeignToken_IsAnAllowlistSoNewErrorsDefaultToOurs(t *testing.T) {
	// 一个将来才会加上的检查,作者没想到还要标注归属。
	future := errors.New("bearer-jwt: some claim constraint added next year")
	if IsForeignToken(future) {
		t.Error("an unmarked error was classified foreign; the classification must be an " +
			"allowlist so that forgetting to mark a new error fails closed (401) rather than " +
			"open (forward the credential upstream)")
	}

	// 关键一条:未标注的 ErrJWTMalformed **不是** foreign。这就是旧实现的漏洞 ——
	// 它把这个哨兵整类当成 foreign,而这个哨兵在验签后也会出现。
	postSig := fmt.Errorf("%w: payload decode to out: some type error", ErrJWTMalformed)
	if IsForeignToken(postSig) {
		t.Error("an ErrJWTMalformed without the foreign marker was classified foreign; " +
			"VerifyHS256JWT returns that sentinel on both sides of hmac.Equal, so the " +
			"sentinel's identity cannot carry the classification")
	}

	// 而带标记的必须是 foreign。
	marked := fmt.Errorf("%w: %w: expected 3 segments", ErrJWTForeign, ErrJWTMalformed)
	if !IsForeignToken(marked) {
		t.Error("a marked pre-signature failure was not classified foreign")
	}
}

// 空密钥报 ErrJWTInvalidSig,那是真·验签前(根本没算签名),必须带标记。
//
// 生产不可达(NewBearerJWTVerifier 拒绝空密钥、newBearerJWTVerifierForTest 返回
// nil),但同一个反转要在这里也成立 —— 否则下一个人接一个允许空密钥的调用方时,
// 每张 token 都会因"不是我们的"被转发上游。
func TestIsForeignToken_EmptySecretIsForeign(t *testing.T) {
	tok := signHS256Raw(t, classificationSecret, `{"alg":"HS256","typ":"JWT"}`,
		fmt.Sprintf(`{"userId":1,"iat":%d,"exp":%d}`,
			time.Now().Unix(), time.Now().Add(time.Hour).Unix()))
	var c bearerJWTClaims
	err := VerifyHS256JWT(tok, nil, time.Now(), &c)
	if err == nil {
		t.Fatal("an empty secret must be refused")
	}
	if !IsForeignToken(err) {
		t.Fatalf("IsForeignToken(%v) = false, want true: with no secret no signature was ever "+
			"computed, so nothing links the token to us", err)
	}
}

// verifyBearerJWT 追加的 claims 约束全在验签之后,必须一律判成"我们的"。
func TestIsForeignToken_ClaimConstraintFailuresAreOurs(t *testing.T) {
	const hdr = `{"alg":"HS256","typ":"JWT"}`
	now := time.Now()

	cases := map[string]string{
		"zero userId": fmt.Sprintf(`{"userId":0,"iat":%d,"exp":%d}`,
			now.Add(-time.Minute).Unix(), now.Add(time.Hour).Unix()),
		"missing iat": fmt.Sprintf(`{"userId":42,"exp":%d}`, now.Add(time.Hour).Unix()),
		"iat in the future": fmt.Sprintf(`{"userId":42,"iat":%d,"exp":%d}`,
			now.Add(48*time.Hour).Unix(), now.Add(72*time.Hour).Unix()),
		"expired": fmt.Sprintf(`{"userId":42,"iat":%d,"exp":%d}`,
			now.Add(-2*time.Hour).Unix(), now.Add(-time.Hour).Unix()),
	}

	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			tok := signHS256Raw(t, classificationSecret, hdr, payload)
			_, err := verifyBearerJWT(tok, []byte(classificationSecret), now, 0)
			if err == nil {
				t.Fatal("expected rejection")
			}
			if IsForeignToken(err) {
				t.Fatalf("IsForeignToken(%v) = true, want false: HMAC already matched, so the "+
					"token is ours and was refused on its own merits", err)
			}
		})
	}
}
