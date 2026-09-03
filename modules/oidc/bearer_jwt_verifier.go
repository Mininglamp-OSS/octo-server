package oidc

// bearer_jwt_verifier.go — 业务后端自签 HS256 JWT 的验签器,及其装配。
//
// 为什么要导出:有两处需要验这种 token —— 本模块的 /exchange-jwt,以及
// modules/integration 的两个端点(桌面客户端手上只有这种凭据)。密钥读取、
// 强度校验、issuer 命名空间派生这三件事一旦在第二处重写,就又是一份会漂移的
// 副本;而这里漂移的后果是身份命名空间不一致 —— 同一个人在两条路径下被认成
// 两个账号,且 (issuer, subject) 落库后不可逆。

import (
	"errors"
	"strings"
	"time"
)

// BearerJWTVerifier 验证业务后端自签的 HS256 JWT 并映射为 IdentityClaims。
//
// 零值不可用;必须经 NewBearerJWTVerifier 构造。
type BearerJWTVerifier struct {
	secret []byte
	issuer string
}

// NewBearerJWTVerifier 从环境装配验签器。
//
// 未配置密钥时返回 (nil, nil) —— 这是合法部署形态("上游 OIDC 开、业务 JWT 不接"),
// 调用方按 nil 表示"这条凭据路径未启用"处理。
//
// 密钥不达标或 issuer 派生失败时返回 error:两者都是运维配置错误,不能静默降级成
// "这条路径不可用",否则桌面端会以一个笼统的 401 表现出来,而真实原因只在启动
// 日志里一闪而过。
func NewBearerJWTVerifier(cfg ProviderConfig) (*BearerJWTVerifier, error) {
	sec := strings.TrimSpace(getString("OCTO_OIDC_BEARER_JWT_SECRET", ""))
	if sec == "" {
		return nil, nil
	}
	// 密钥强度是准入条件而非调优项:持有它就能为任意 userId 签一张换会话的 token。
	if err := validateBearerJWTSecret([]byte(sec)); err != nil {
		return nil, err
	}
	iss, err := bearerJWTIssuerFromUpstream(cfg.Issuer)
	if err != nil {
		return nil, err
	}
	return &BearerJWTVerifier{secret: []byte(sec), issuer: iss}, nil
}

// Issuer 这条路径的身份命名空间(上游 issuer + "#bearer-jwt")。
//
// 与上游 issuer 隔离是必须的:业务 JWT 的 userId 与上游 subject 是两套 ID 空间,
// 共用命名空间会让 (issuer, subject) 把两个不同的人合成一个账号。
func (v *BearerJWTVerifier) Issuer() string { return v.issuer }

// SecretLen 供启动日志用,不回显密钥本身。
func (v *BearerJWTVerifier) SecretLen() int { return len(v.secret) }

// Verify 验签并映射为 claims。
//
// 失败即返回 error;调用方对客户端只回一个笼统的 401(反枚举)。
//
// **这个方法同时承担"这是不是一张业务 JWT"的判定职责。** 判定依据是验签结果,
// 不是 token 的形态:一张 token 要么带着我方密钥下的合法 HMAC,要么没有,这是
// 确定性的检验而不是启发式猜测。因此调用方可以在同一个 Authorization 头上先试
// 这条路径、失败再回落到上游凭据路径,而不构成"按形态分流"。
//
// 两个方向都不会误判:
//   - 上游的不透明 access_token 不可能带出我方密钥下的合法签名(那需要知道密钥);
//   - 上游的 id_token 是 RS256,而 VerifyHS256JWT 把 alg 钉死为 HS256 并显式拒绝
//     RS256,所以不会走算法混淆那条路。
func (v *BearerJWTVerifier) verify(raw string, now time.Time, maxAge time.Duration) (*IdentityClaims, error) {
	claims, err := verifyBearerJWT(strings.TrimSpace(raw), v.secret, now, maxAge)
	if err != nil {
		return nil, err
	}
	return claims.toIdentityClaims(v.issuer), nil
}

// VerifyForRedemption 用于"出示一次、换成我方会话"的场景(/exchange-jwt)。
//
// 除签名与 exp 之外另加一道 bearerJWTMaxAge 的新鲜度上限:上游给的 exp 约 15 天,
// 而这个用途只需要"刚登录完"那一小段,把可重放窗口压到分钟级。
func (v *BearerJWTVerifier) VerifyForRedemption(raw string, now time.Time) (*IdentityClaims, error) {
	return v.verify(raw, now, bearerJWTMaxAge)
}

// VerifyForAuthentication 用于**每次请求都出示同一张 token** 的常驻认证器
// (modules/integration 的两个端点)。
//
// 生命周期用 token 自己的 exp,不套 VerifyForRedemption 那道分钟级上限:桌面客户端
// 登录后把这张 JWT 存进本地并长期复用,并不会每次重签。把一次性兑换的上限套在
// 认证器上,结果是登录十分钟后端点永久返回 401,而且与"凭据无效"不可区分。
//
// 换来的代价是这条路径的可重放窗口等于 exp(约 15 天)。这不是本方法引入的问题,
// 而是"上游 JWT 没有 aud/jti"这条已记在 Pending 的约束的直接后果;用一个会弄坏
// 功能的上限去掩盖它,并不会让凭据更安全。
func (v *BearerJWTVerifier) VerifyForAuthentication(raw string, now time.Time) (*IdentityClaims, error) {
	return v.verify(raw, now, 0)
}

// newBearerJWTVerifierForTest 直接注入密钥与 issuer,绕过环境变量。
//
// 放在生产文件而非 _test.go 里:modules/integration 的测试也需要它,而 Go 的
// 测试文件不跨包可见。它不做强度校验,所以**只应由测试调用** —— 生产路径必须
// 走 NewBearerJWTVerifier,那条路径会拒绝弱密钥。
func newBearerJWTVerifierForTest(secret []byte, issuer string) *BearerJWTVerifier {
	if len(secret) == 0 || issuer == "" {
		return nil
	}
	return &BearerJWTVerifier{secret: secret, issuer: issuer}
}

// IsForeignToken 报告一个 Verify 错误是否意味着"这张 token 不是我们签的"。
//
// 判定依据是 **ErrJWTForeign 标记**,而不是错误哨兵的身份。
//
// 曾经这里按哨兵身份判:ErrJWTMalformed / ErrJWTBadAlg / ErrJWTInvalidSig 三者
// 视为"不是我们的"。**那个前提是假的** —— ErrJWTMalformed 横跨 hmac.Equal 两侧:
// 段数/base64 不对是验签前(确实无法归属),而 "payload json"、"exp 不是整数"、
// "payload decode to out" 三处出现在验签**通过之后**,却报同一个哨兵。于是一张
// 带着我方合法 HMAC、只是 payload 字段类型写错的 token(JS 后端写
// `iat: Date.now()/1000` 不取整就是这个形态)会被判成"别人的",回落转发给上游 ——
// 而上游那条路径把凭据放在 URL query 上,于是载荷里的 PII 加一份在我方密钥下
// 合法的签名一起落进第三方访问日志。后者是可离线爆破密钥的材料,正是本模块给
// 密钥设 32 字节下限时所防的东西。
//
// 所以判定必须由**产生错误的位置**显式标注,并且方向要反过来 —— 白名单:
//
//   - 只有验签前/验签本身的失败被标 ErrJWTForeign,只有它们算"不是我们的";
//   - 其余一切(含将来新增的 claims 约束、含忘了标注的新错误)默认算"是我们的",
//     调用方就地拒绝、不转发。
//
// 黑名单会让"忘记标注"fail-open(转发),白名单让它 fail-closed(401)。
// 这个区分放在这里而不是让每个调用方拼错误列表,是因为漏一个的后果是泄漏凭据。
func IsForeignToken(err error) bool {
	return errors.Is(err, ErrJWTForeign)
}
