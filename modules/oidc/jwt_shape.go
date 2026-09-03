package oidc

// jwt_shape.go — "这个值是不是 JWT 形态" 的判定,以及它唯一的合法用途。
//
// **这不是凭据分流。** 分流靠 HMAC(见 IsForeignToken),因为那是确定性的;
// 按形态猜该走哪条路是本模块明确拒绝的做法。
//
// 这里的用途只有一个:凡是**验不过或无法验**的 JWT,都不能被回落转发给上游 ——
// 那条路把凭据放在 URL query 上。如果 provider 声明了 OpaqueClientCredential
// (它的 access_token 是不透明串),那么一张 JWT **不可能**是这条路上的合法凭据:
// 转发它不可能成功,只可能把载荷里的 PII 和一份在某把我方密钥下合法的签名送进
// 第三方的访问日志。
//
// "验不过"包含两种,两种都要挡:我方没配密钥(无从验),以及配了但对不上
// (最现实的是**密钥轮换**窗口里用旧密钥签的 token —— 那些确实是我方签发的)。
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

// JWTShapedCredentialMustNotBeForwarded 判断"这张凭据是否绝不能被外发"。
//
// 条件只有两个,**都与我方是否配置了密钥无关**:
//   - provider 声明 OpaqueClientCredential —— 它的 access_token 是不透明串;
//   - 值是 JWT 形态。
//
// 上一版还多一个 `!verifierConfigured` 门控,那是个错误:论证是"JWT 不可能是这个
// 上游的合法凭据",这件事无条件成立,门控让它只覆盖了一半。漏掉的那半最现实的
// 触发是**密钥轮换** —— 窗口期里客户端手上还有用旧密钥签的 token,它们确确实实
// 是我方签发的,HMAC 对不上新密钥就被判 foreign、然后外发。于是"我方签发的凭据
// 绝不外发"这条不变量每次轮换都破一次。
//
// **调用顺序:必须排在 HMAC 验签之后。** 一张有效的业务 JWT 也是 JWT 形态。
//
// 两扇门上这条的后果不同,值得说清:
//   - modules/integration 是**常驻认证器**,有效的业务 JWT 必须**通过**。顺序写反
//     就把桌面端整条路拒掉 —— 那里用 `claims == nil &&` 做前置条件,
//     并由 TestBearerJWT_SpacesAndExchangeWork 钉住(去掉前置条件即红)。
//   - /exchange 上业务 JWT 两种顺序都会被拒(该发 /exchange-jwt),所以顺序只影响
//     上报的原因:own_business_jwt(认出是我们的)还是 unverifiable_jwt(只知道是
//     JWT 形态)。对客户端不可分,但排障时是两回事。
func JWTShapedCredentialMustNotBeForwarded(caps ProviderCapabilities, raw string) bool {
	if !caps.OpaqueClientCredential {
		return false
	}
	return looksLikeJWT(raw)
}
