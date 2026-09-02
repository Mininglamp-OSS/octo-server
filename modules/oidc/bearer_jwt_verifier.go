package oidc

// bearer_jwt_verifier.go — 业务后端自签 HS256 JWT 的验签器,及其装配。
//
// 为什么要导出:有两处需要验这种 token —— 本模块的 /exchange-jwt,以及
// modules/integration 的两个端点(桌面客户端手上只有这种凭据)。密钥读取、
// 强度校验、issuer 命名空间派生这三件事一旦在第二处重写,就又是一份会漂移的
// 副本;而这里漂移的后果是身份命名空间不一致 —— 同一个人在两条路径下被认成
// 两个账号,且 (issuer, subject) 落库后不可逆。

import (
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
func (v *BearerJWTVerifier) Verify(raw string, now time.Time) (*IdentityClaims, error) {
	claims, err := verifyBearerJWT(strings.TrimSpace(raw), v.secret, now)
	if err != nil {
		return nil, err
	}
	return claims.toIdentityClaims(v.issuer), nil
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
