package oidc

// jwt_hs256.go — 通用 HS256 JWT 验签工具。
//
// 为什么不直接用 golang-jwt/jwt 库:库默认接受多种 alg,调用方必须显式配
// ValidMethods 才能防 alg 混淆攻击(经典 JWT 漏洞:RS256 公钥被当 HMAC key 用);
// 而我们所有需要 JWT 验签的场景都**只有** HS256(对称密钥),直接在函数里写死
// "只接受 HS256" 能把这个安全默认值固化在类型签名里,不存在忘配的可能。
//
// 本文件不引第三方依赖,总代码量 <80 行;Claims 结构体由调用方定义,通过 json.Unmarshal
// 填充(和 json.NewDecoder.Decode 同一模式),不替调用方决定字段形状。

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 错误哨兵。调用方用 errors.Is 判类,不要比较错误字符串。
var (
	// ErrJWTMalformed token 不是 "a.b.c" 三段、base64 解码失败、header/payload 不是合法 JSON。
	ErrJWTMalformed = errors.New("jwt: malformed token")
	// ErrJWTBadAlg header.alg 不是 "HS256" 或 header.typ 异常(拒绝 none/RS256/ES256 等混淆攻击)。
	ErrJWTBadAlg = errors.New("jwt: alg must be HS256")
	// ErrJWTInvalidSig HMAC-SHA256 签名不匹配。
	ErrJWTInvalidSig = errors.New("jwt: invalid signature")
	// ErrJWTExpired payload.exp 存在且早于 now。
	ErrJWTExpired = errors.New("jwt: token expired")
	// ErrJWTMissingExp payload 不含 exp 字段或类型异常。强制要求 exp 是为了
	// 让"永久有效 token"不可能被签发出来。
	ErrJWTMissingExp = errors.New("jwt: exp claim is required")
	// ErrJWTForeign 标注"该失败发生在验签通过之前(含验签本身)",因此无法断定
	// 这张 token 与我方密钥有关。见 IsForeignToken。
	ErrJWTForeign = errors.New("jwt: token cannot be attributed to our key")
)

// joseHeader 固定只认 {"alg":"HS256","typ":"JWT"}。多余字段允许但忽略。
type joseHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// VerifyHS256JWT 解析并校验一个 HS256 JWT,验签通过后把 payload 反序列化到 out。
//
// 行为(全部是安全默认,调用方无法关闭):
//   - token 必须正好三段 (header.payload.signature);
//   - header.alg 必须是 "HS256"(大小写敏感),header.typ 若存在必须是 "JWT";
//   - 签名用 HMAC-SHA256 计算,比较用 hmac.Equal 防时序侧信道;
//   - payload.exp 必须存在,必须是 JSON 数字(Unix 秒),且必须 > now(零宽);
//   - payload 通过 json.Unmarshal 填充到 out(out 必须是非 nil 指针)。
//
// 本函数不校验 iss/aud/sub:这些语义是调用方(IdP 适配器)的责任。它只保证
// "这个 token 是持有 secret 的一方签的、还没过期、是 HS256、结构合法"。
func VerifyHS256JWT(token string, secret []byte, now time.Time, out any) error {
	if out == nil {
		// 刻意**不**标 ErrJWTForeign:这是调用方的编程错误,签名根本没算过,
		// 归属未知。未标记 = 按"是我们的"处理 = 不转发上游,这是安全的那一侧。
		return fmt.Errorf("%w: out destination is nil", ErrJWTMalformed)
	}
	if len(secret) == 0 {
		// 空密钥的 HMAC 任何人都能离线算,等价于完全不验签,明确拒绝而不是"验证通过"。
		// 生产装配路径(api.go New/bearerJWTSecret 配置)也有非空检查,这里是纵深防御。
		return fmt.Errorf("%w: %w: secret must be non-empty", ErrJWTForeign, ErrJWTInvalidSig)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return fmt.Errorf("%w: %w: expected 3 segments, got %d", ErrJWTForeign, ErrJWTMalformed, len(parts))
	}
	rawHeader, rawPayload, rawSig := parts[0], parts[1], parts[2]

	// 解码 header 并校验 alg 必须是 HS256(抗 alg 混淆攻击的关键一步)。
	headerBytes, err := b64urlDecode(rawHeader)
	if err != nil {
		return fmt.Errorf("%w: %w: header base64: %v", ErrJWTForeign, ErrJWTMalformed, err)
	}
	var hdr joseHeader
	if err := json.Unmarshal(headerBytes, &hdr); err != nil {
		return fmt.Errorf("%w: %w: header json: %v", ErrJWTForeign, ErrJWTMalformed, err)
	}
	if hdr.Alg != "HS256" {
		return fmt.Errorf("%w: %w: got %q", ErrJWTForeign, ErrJWTBadAlg, hdr.Alg)
	}
	if hdr.Typ != "" && hdr.Typ != "JWT" {
		return fmt.Errorf("%w: %w: typ %q is not JWT", ErrJWTForeign, ErrJWTBadAlg, hdr.Typ)
	}

	// 解码 payload。先解码后验签是 JWT 库的常规做法(签名校验覆盖 header+payload 原文,
	// 早解 payload 不会引入"未授权数据被处理"的问题,因为调用方必须等 err==nil 才用 out)。
	payloadBytes, err := b64urlDecode(rawPayload)
	if err != nil {
		return fmt.Errorf("%w: %w: payload base64: %v", ErrJWTForeign, ErrJWTMalformed, err)
	}

	// 验签:签名输入是 ASCII(header) + "." + ASCII(payload),签名是 raw signature 的 base64url 解码。
	signingInput := rawHeader + "." + rawPayload
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signingInput))
	expectedSig := mac.Sum(nil)
	actualSig, err := b64urlDecode(rawSig)
	if err != nil {
		return fmt.Errorf("%w: %w: signature base64: %v", ErrJWTForeign, ErrJWTMalformed, err)
	}
	if !hmac.Equal(actualSig, expectedSig) {
		return fmt.Errorf("%w: %w", ErrJWTForeign, ErrJWTInvalidSig)
	}

	// ==================== 以下都在验签通过之后 ====================
	//
	// 这条线以下的任何失败都意味着 **token 确实是持有我方密钥的一方签的**,只是
	// 按 claims 自身的条件被拒。一律**不**标 ErrJWTForeign —— 见 IsForeignToken:
	// 未标记即"是我们的",调用方因此不会把它转发给上游 IdP。
	//
	// 新增检查时不需要记得做任何事,这就是把标记放在上半段的目的。

	// 用 RawMessage 单独取 exp,再按数字解析——避免依赖 json.Number/UseNumber 模式,
	// 也能精确拒绝字符串/布尔/对象等非数字类型。
	var lens struct {
		Exp json.RawMessage `json:"exp"`
	}
	if err := json.Unmarshal(payloadBytes, &lens); err != nil {
		return fmt.Errorf("%w: payload json: %v", ErrJWTMalformed, err)
	}
	if len(lens.Exp) == 0 || string(lens.Exp) == "null" {
		return ErrJWTMissingExp
	}
	var expSec int64
	if err := json.Unmarshal(lens.Exp, &expSec); err != nil {
		return fmt.Errorf("%w: exp is not an integer: %v", ErrJWTMalformed, err)
	}
	if !now.Before(time.Unix(expSec, 0)) {
		return ErrJWTExpired
	}

	if err := json.Unmarshal(payloadBytes, out); err != nil {
		return fmt.Errorf("%w: payload decode to out: %v", ErrJWTMalformed, err)
	}
	return nil
}

// b64urlDecode 解码 base64url(raw base64,无 padding,和 JWT 规范一致)。
// 使用 Strict() 拒绝非规范编码(末字符未使用比特非零)——同一签名的多个字符串
// 变体虽然不能被用来伪造签名,但会破坏"token 字符串唯一性"假设(防重放/去重/
// 审计比对)。加上 Strict 成本为零,默认开启。
func b64urlDecode(s string) ([]byte, error) {
	return base64.RawURLEncoding.Strict().DecodeString(s)
}
