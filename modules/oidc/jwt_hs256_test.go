package oidc

// jwt_hs256_test.go — 通用 HS256 JWT 验签函数的单元测试。
//
// 测试用例全部使用合成密钥与合成 token(由测试代码自己用 crypto/hmac 签出),
// 不含任何真实凭据或真实用户 PII。

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

// signJWT 用 secret 签一个合法 HS256 JWT(header 固定 {"alg":"HS256","typ":"JWT"})。
func signJWT(t *testing.T, secret string, payloadObj any) string {
	t.Helper()
	hb, err := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	pb, err := json.Marshal(payloadObj)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	rh := base64.RawURLEncoding.EncodeToString(hb)
	rp := base64.RawURLEncoding.EncodeToString(pb)
	input := rh + "." + rp
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(input))
	rs := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return input + "." + rs
}

// signJWTWithRawHeader 允许自定义 header(用于 alg/typ 测试),payloadB64 是已编码段。
func signJWTWithRawHeader(t *testing.T, secret, headerB64, payloadB64 string) string {
	t.Helper()
	input := headerB64 + "." + payloadB64
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(input))
	rs := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return input + "." + rs
}

// ---- Happy path 与基础形状校验 ----------------------------------------------

func TestVerifyHS256JWT_HappyPath_CustomClaims(t *testing.T) {
	type custom struct {
		Sub string `json:"sub"`
		Exp int64  `json:"exp"`
	}
	secret := "unit-test-secret-32bytes-xxxxx"
	payload := custom{Sub: "s1", Exp: time.Now().Add(time.Hour).Unix()}
	tok := signJWT(t, secret, payload)
	var out custom
	if err := VerifyHS256JWT(tok, []byte(secret), time.Now(), &out); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if out.Sub != "s1" {
		t.Errorf("sub=%q, want s1", out.Sub)
	}
}

func TestVerifyHS256JWT_SignatureUsesSigningInputFormat(t *testing.T) {
	secret := "correct-secret-32bytes-xxxxxxx"
	wrong := "wrong-secret-32bytes-xxxxxxxxx"
	payload := map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "sub": "x"}
	good := signJWT(t, secret, payload)
	bad := signJWT(t, wrong, payload)
	var out struct {
		Sub string `json:"sub"`
	}
	if err := VerifyHS256JWT(bad, []byte(secret), time.Now(), &out); !errors.Is(err, ErrJWTInvalidSig) {
		t.Errorf("bad sig: got %v, want ErrJWTInvalidSig", err)
	}
	if err := VerifyHS256JWT(good, []byte(secret), time.Now(), &out); err != nil {
		t.Errorf("good sig: %v", err)
	}
}

func TestVerifyHS256JWT_NowExactlyAtExp(t *testing.T) {
	secret := "s"
	exp := time.Now().Add(time.Hour).Truncate(time.Second).Unix()
	tok := signJWT(t, secret, map[string]any{"exp": exp})
	var out struct{}
	// now == exp 严格过期(零宽)。
	if err := VerifyHS256JWT(tok, []byte(secret), time.Unix(exp, 0), &out); !errors.Is(err, ErrJWTExpired) {
		t.Errorf("now==exp: got %v, want ErrJWTExpired", err)
	}
	if err := VerifyHS256JWT(tok, []byte(secret), time.Unix(exp, 0).Add(-1), &out); err != nil {
		t.Errorf("now=exp-1ns: unexpected err %v", err)
	}
}

func TestVerifyHS256JWT_ExpNotANumber(t *testing.T) {
	secret := "s"
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":"9999999999"}`))
	tok := signJWTWithRawHeader(t, secret, header, payload)
	var out struct{}
	err := VerifyHS256JWT(tok, []byte(secret), time.Now(), &out)
	if !errors.Is(err, ErrJWTMalformed) {
		t.Errorf("exp as string: got %v, want ErrJWTMalformed", err)
	}
}

func TestVerifyHS256JWT_TypCaseAndOmission(t *testing.T) {
	secret := "s"
	// typ 省略:允许(非强制字段)。
	hb, _ := json.Marshal(map[string]string{"alg": "HS256"})
	pb, _ := json.Marshal(map[string]any{"exp": time.Now().Add(time.Hour).Unix()})
	rh := base64.RawURLEncoding.EncodeToString(hb)
	rp := base64.RawURLEncoding.EncodeToString(pb)
	input := rh + "." + rp
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(input))
	tokNoTyp := input + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	var out struct{}
	if err := VerifyHS256JWT(tokNoTyp, []byte(secret), time.Now(), &out); err != nil {
		t.Errorf("typ omitted: unexpected err %v", err)
	}
	// typ="jwt"(小写)→ 拒(精确匹配 "JWT")。
	hb2, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "jwt"})
	rh2 := base64.RawURLEncoding.EncodeToString(hb2)
	mac2 := hmac.New(sha256.New, []byte(secret))
	mac2.Write([]byte(rh2 + "." + rp))
	tokLowerTyp := rh2 + "." + rp + "." + base64.RawURLEncoding.EncodeToString(mac2.Sum(nil))
	if err := VerifyHS256JWT(tokLowerTyp, []byte(secret), time.Now(), &out); !errors.Is(err, ErrJWTBadAlg) {
		t.Errorf("typ=jwt: got %v, want ErrJWTBadAlg", err)
	}
}

// 用合成的测试密钥 + 合成 token 做端到端验签。
//
// 所有值均为测试专用假数据,不关联任何真实凭据或真实用户。
func TestVerifyHS256JWT_RealisticShapeToken(t *testing.T) {
	const testSecret = "test-jwt-secret-not-real-12345678"
	// 由 testSecret 签出的合成 JWT,claims 用合成 userId=54321/domainAccount=test.user。
	const synthToken = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." +
		"eyJkb21haW5BY2NvdW50IjoidGVzdC51c2VyIiwiZXhwIjoxNzg5NTUzMzI2LCJpYXQiOjE3ODgyNTczMjYs" +
		"InBheWxvYWRIYXNoIjoiZGVhZGJlZWYwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMCIsInVzZXJJZCI6NTQzMjF9." +
		"xUaj-o13OsmFu3FV9caHO5E17Ec7ZERbrXirZucT9QY"

	var claims bearerJWTClaims
	safeNow := time.Unix(1788257326+60, 0)
	if err := VerifyHS256JWT(synthToken, []byte(testSecret), safeNow, &claims); err != nil {
		t.Fatalf("synthetic token verify failed: %v", err)
	}
	if claims.UserID != 54321 || claims.DomainAccount != "test.user" {
		t.Errorf("claims mismatch: %+v", claims)
	}

	expiredNow := time.Unix(1789553326+1, 0)
	err := VerifyHS256JWT(synthToken, []byte(testSecret), expiredNow, &claims)
	if !errors.Is(err, ErrJWTExpired) {
		t.Errorf("expired+1s: got %v, want ErrJWTExpired", err)
	}

	wrongSecret := []byte("wrong-key-not-the-one-above-123")
	if err := VerifyHS256JWT(synthToken, wrongSecret, safeNow, &claims); !errors.Is(err, ErrJWTInvalidSig) {
		t.Errorf("wrong secret: got %v, want ErrJWTInvalidSig", err)
	}
}

// 空密钥等价于"任何人都能离线计算 HMAC",必须在入口就拒绝。
func TestVerifyHS256JWT_EmptySecretRejected(t *testing.T) {
	err := VerifyHS256JWT("a.b.c", []byte{}, time.Now(), &struct{}{})
	if err == nil {
		t.Fatal("empty secret must be rejected")
	}
	if !errors.Is(err, ErrJWTInvalidSig) {
		t.Errorf("empty secret: got %v, want ErrJWTInvalidSig wrapped", err)
	}
}

// 非规范 base64(末字符冗余比特非零)必须被 Strict 解码拒绝——否则一个签名对应多个
// 字符串变体,破坏"token 字符串唯一"假设(防重放/去重/审计)。
func TestVerifyHS256JWT_NonCanonicalBase64Rejected(t *testing.T) {
	secret := []byte("s")
	tok := signJWT(t, "s", map[string]any{"exp": time.Now().Add(time.Hour).Unix()})
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("setup: unexpected segment count")
	}
	// 把签名段末字符替换为可能产生相同解码字节的候选字符。base64 每组 4 字符编码 3 字节,
	// 最后一组 4 字符编码的末尾字节的低 2 bit 会被丢弃——因此某些替换下非 Strict 解码
	// 会得到完全相同的 32 字节签名。
	last := parts[2][len(parts[2])-1]
	foundVariant := false
	alphabet := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	for i := 0; i < len(alphabet); i++ {
		c := alphabet[i]
		if c == last {
			continue
		}
		mangled := parts[0] + "." + parts[1] + "." + parts[2][:len(parts[2])-1] + string(c)
		if err := VerifyHS256JWT(mangled, secret, time.Now(), &struct{}{}); err == nil {
			t.Errorf("non-canonical signature (last=%q) accepted; must be rejected by Strict decode", c)
			foundVariant = true
			break
		}
	}
	if !foundVariant {
		// 备用:加入明显非法字符,保证 Strict 路径确实启用。
		bad := parts[0] + "." + parts[1] + "." + parts[2] + "!!!"
		if err := VerifyHS256JWT(bad, secret, time.Now(), &struct{}{}); err == nil {
			t.Error("ill-formed base64 was accepted")
		}
	}
}
