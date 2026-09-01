package oidc

// bearer_jwt.go — 客户端自签 bearer JWT 的 claims 结构与到 IdentityClaims 的映射。
//
// 本文件只做"字段形状 + 映射"两件事;验签走通用的 VerifyHS256JWT(jwt_hs256.go),
// 不在这里重复签名/过期/alg 校验。这样将来接入第二个 HS256 IdP(如果有的话),
// 只需再加一个 claims 结构体和 toIdentityClaims 方法,验签逻辑零复制。

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// bearerJWTClaims 客户端自签 JWT payload(已通过验签 + 过期校验)。
//
// 字段形状(测试样本,真实 payload 仅 userId/domainAccount 字段名与此一致,数字取值
// 会随用户变化):
//
//	{"userId":<int>,"domainAccount":<string>,"payloadHash":<hex-sha256>,
//	 "iat":<unixsec>,"exp":<unixsec>}
//
// 字段使用:
//   - UserID 是 客户端侧用户主键(整数),必须非 0。
//   - DomainAccount 是域账号(如 "name.lastname" 这种登录名),仅作显示与审计,
//     不做主键(可能改名/轮换,且未验证是否全局唯一)。
//   - PayloadHash 是 客户端给"后续附加 userData"的完整性摘要,不是我们的 key,
//     不影响鉴权,仅保留以便审计。
//   - Iat 签发时间,未被消费,仅保留以便排障。
type bearerJWTClaims struct {
	UserID        int64  `json:"userId"`
	DomainAccount string `json:"domainAccount"`
	PayloadHash   string `json:"payloadHash"`
	Iat           int64  `json:"iat"`
	// Exp 由通用 VerifyHS256JWT 单独解析并强制校验,这里只是让 json.Unmarshal
	// 有个落点(否则 strict 模式会报未知字段,但我们没用 DisallowUnknownFields,
	// 所以这行其实可选,写上让阅读者一眼看到该字段存在)。
	Exp int64 `json:"exp"`
}

// 客户端侧特有的 claims 校验错误。通用的 malformed/alg/sig/exp 错误走 jwt_hs256.go 的哨兵。
var (
	// ErrBearerJWTNoUserID payload.userId 缺失或为 0(可能是未认证/默认值哨兵)。
	ErrBearerJWTNoUserID = errors.New("bearer-jwt: userId is required and must be non-zero")
)

// verifyBearerJWT 验签 bearer JWT 并解析 claims。secret 是 HS256 对称密钥(字节原样传入;
// 调用方负责做 hex/ascii 等解码——该密钥是 ASCII 字符串,直接 []byte(secret) 即可,
// 具体长度/取值由运维通过环境变量注入,不在代码里硬编码)。
//
// 本函数是 bearer JWT 路径的唯一验签入口。通用 JWT 校验(alg/签名/过期/段数)走
// VerifyHS256JWT,这里只追加 本路径特有的 claims 约束(userId 必须存在且非零)。
func verifyBearerJWT(token string, secret []byte, now time.Time) (*bearerJWTClaims, error) {
	var c bearerJWTClaims
	if err := VerifyHS256JWT(token, secret, now, &c); err != nil {
		return nil, err
	}
	if c.UserID <= 0 {
		return nil, ErrBearerJWTNoUserID
	}
	return &c, nil
}

// toIdentityClaims 把客户端 claims 转成协议中立的 IdentityClaims。
//
// issuer 由调用方注入(bearerJWTIssuerFromUpstream 的派生值)。Subject 用
// strconv.FormatInt(userId,10) —— 客户端 userId 是整数,而 IdentityClaims.Subject
// 是字符串(对齐上游 IdP sub 18 位长数字串),两 issuer 的主键类型都收敛成字符串便于落库。
//
// 故意不填 Email/Phone/Verified 位:bearer JWT 不携带这些字段,Verified 留零值
// (false) 让 autolink fail-closed,避免误开自动绑号面;Name 用 DomainAccount 做
// 兜底显示(不是邮箱/手机,不会撞 autolink 条件)。
func (c *bearerJWTClaims) toIdentityClaims(issuer string) *IdentityClaims {
	if c == nil {
		return nil
	}
	return &IdentityClaims{
		Issuer:  issuer,
		Subject: strconv.FormatInt(c.UserID, 10),
		Name:    c.DomainAccount,
		// Email/Phone/EmailVerified/PhoneVerified/Nonce 全部留零值。
	}
}

// bearerJWTIssuerSuffix 追加在上游 issuer 之后,构成 bearer JWT 路径的 issuer 命名空间。
//
// 用 '#' 起头:合法的 issuer 是一个不带 fragment 的绝对 URI,所以 '#' 不可能出现在
// 上游 issuer 里 —— 后缀与上游值永不歧义,也不会有某个上游 issuer 恰好等于另一个
// 上游 issuer + 后缀的情况。
const bearerJWTIssuerSuffix = "#bearer-jwt"

// issuerMaxLen 与 user_oidc_identity.issuer 的列宽一致(VARCHAR(255))。
// 超长会在 INSERT 时被 MySQL 截断或报错,取决于 sql_mode —— 截断更危险:
// 两个不同 issuer 可能截成同一个值,uk_issuer_subject 就把两个人合成一个号。
// 所以在启动期拒绝,而不是等运行期。
const issuerMaxLen = 255

// bearerJWTIssuerFromUpstream 由已配置的上游 issuer 派生 bearer JWT 的 issuer 命名空间。
//
// 为什么派生而不是单独给一个 env:
//   - bearer JWT 的 userId 与上游 IdP 的 sub 是两套 ID 空间,必须落在不同 issuer 下,
//     否则 (issuer, subject) 这个主键会把两个人的身份混在一起;
//   - 而"测试/生产必须用不同 issuer"这条约束,上游 issuer 本身已经满足(它每环境
//     一个值,是必填项且启动期校验)。派生因此天然继承了环境隔离,不需要运维再配
//     第二个环境标识 —— 少一个 env,也少一类"两个环境标识配得不一致"的误配。
//
// 该值一旦上线不可更改(改了等于该路径全员重建号)。
func bearerJWTIssuerFromUpstream(upstreamIssuer string) (string, error) {
	u := strings.TrimSpace(upstreamIssuer)
	if u == "" {
		return "", fmt.Errorf("bearer-jwt: upstream issuer (DM_OIDC_PROVIDER_ISSUER) is empty; " +
			"the bearer JWT issuer namespace is derived from it")
	}
	if strings.Contains(u, bearerJWTIssuerSuffix) {
		return "", fmt.Errorf("bearer-jwt: upstream issuer %q already contains %q; "+
			"it must be the plain upstream issuer, not an already-derived value", u, bearerJWTIssuerSuffix)
	}
	iss := u + bearerJWTIssuerSuffix
	if len(iss) > issuerMaxLen {
		return "", fmt.Errorf("bearer-jwt: derived issuer is %d bytes, exceeds the %d-byte column width "+
			"(upstream issuer %q is too long)", len(iss), issuerMaxLen, u)
	}
	return iss, nil
}
