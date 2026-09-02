package oidc

// jwt_shape.go — "这个值是不是 JWT 形态" 的判定,以及它唯一的合法用途。
//
// **这不是凭据分流。** 分流靠 HMAC(见 IsForeignToken),因为那是确定性的;
// 按形态猜该走哪条路是本模块明确拒绝的做法。
//
// 这里的用途只有一个,而且只在一个条件下成立:当业务 JWT 验签器**未配置**时,
// 我方没有密钥,无法对一张 JWT 做任何归属判定,于是它会被回落转发给上游 ——
// 而上游那条路把凭据放在 URL query 上。此时如果 provider 声明了
// OpaqueClientCredential(它的 access_token 是不透明串),那么一张 JWT
// **不可能**是这条路上的合法凭据:转发它不可能成功,只可能把载荷里的 PII 和
// 一份在客户端密钥下合法的签名送进第三方的访问日志。
//
// 所以这是"证明它不可能成功",不是"猜它是什么"。判据挂在能力位上,因为
// "access_token 是不透明串"是厂商事实而非协议事实 —— 标准 OIDC 下客户端
// 出示的凭据本身就是 JWT。

import (
	"encoding/json"
	"strings"
)

// looksLikeJWT 报告 raw 是否是一个 JOSE 紧凑序列化形态的值。
//
// 判据刻意保守:三段、句点分隔、第一段能 base64url 解码成含非空 alg 的 JSON 对象。
// 不看签名、不看 payload —— 那些需要密钥或会把判定变成载荷猜测。
//
// 保守的方向是安全的:漏判(返回 false)只会退回到原有行为(转发),
// 误判(返回 true)会拒绝一个本来也不可能在这条路上成功的值。
func looksLikeJWT(raw string) bool {
	parts := strings.Split(strings.TrimSpace(raw), ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
	}
	headerBytes, err := b64urlDecode(parts[0])
	if err != nil {
		return false
	}
	var hdr joseHeader
	if err := json.Unmarshal(headerBytes, &hdr); err != nil {
		return false
	}
	return strings.TrimSpace(hdr.Alg) != ""
}

// unverifiableJWTMustNotBeForwarded 判断"这张凭据应不应该在外呼前就地拒绝"。
//
// 三个条件必须同时成立:
//   - 验签器未配置(密钥缺失)—— 配置正常时 HMAC 是决定性判据,不需要看形态;
//   - provider 声明 OpaqueClientCredential —— 否则 JWT 是这条路的正常凭据;
//   - 值确实是 JWT 形态。
func UnverifiableJWTMustNotBeForwarded(verifierConfigured bool, caps ProviderCapabilities, raw string) bool {
	if verifierConfigured || !caps.OpaqueClientCredential {
		return false
	}
	return looksLikeJWT(raw)
}
