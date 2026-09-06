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
	// ErrJWTTooOld token 的 iat 距今超过调用方传入的 maxAge。
	//
	// 生产路径当前两个调用方都传 0(不设上限)—— 新鲜度改由兑换台账按**兑换
	// 行为**判定,见 redemption_ledger.go。这里保留能力与哨兵:它是纯函数的一个
	// 通用参数,将来若要给某条路径重新加 iat 上限,不必再实现一遍。
	ErrJWTTooOld = errors.New("bearer-jwt: token was issued too long ago")
	// ErrJWTMissingIat payload 无 iat,无法判断新鲜度。
	ErrJWTMissingIat = errors.New("bearer-jwt: iat claim is required")
	// ErrJWTIatInFuture iat 超出时钟偏移容忍范围地位于未来。
	ErrJWTIatInFuture = errors.New("bearer-jwt: iat is too far in the future")
	// ErrBearerJWTSecretTooShort HS256 密钥强度不足。
	ErrBearerJWTSecretTooShort = errors.New("bearer-jwt: shared secret is too short")
)

const (
	// bearerJWTMinSecretBytes HS256 共享密钥的最小长度。
	//
	// 32 字节对齐同模块 DM_OIDC_RT_ENC_KEY 的既有要求(config.go 强制 AES-256
	// 恰好 32 字节,不满足直接拒绝启动)。理由在这里更强:持有这把密钥就能为
	// **任意** userId 签一张能换会话的 token,而短密钥可被离线爆破 —— 攻击者
	// 只要拿到一张合法 JWT 就能在本地穷举出密钥,此后可以伪造任何人的登录。
	bearerJWTMinSecretBytes = 32

	// bearerJWTClockSkew 容忍签发方与我方的时钟不同步。
	//
	// iat 略微在未来是常态,不该当成攻击;但远期 iat 要拒 —— 那是把 token 的
	// 可用窗口整体往后推。
	bearerJWTClockSkew = 60 * time.Second
)

// validateBearerJWTSecret 校验共享密钥强度。启动期调用,不足则不开启该端点。
//
// 错误信息只给长度,不回显密钥本身 —— 它会进日志。
func validateBearerJWTSecret(secret []byte) error {
	if len(secret) < bearerJWTMinSecretBytes {
		return fmt.Errorf("%w: got %d bytes, need at least %d "+
			"(a short HMAC key can be recovered offline from a single valid token, "+
			"after which any user's login can be forged)",
			ErrBearerJWTSecretTooShort, len(secret), bearerJWTMinSecretBytes)
	}
	return nil
}

// verifyBearerJWT 验签 bearer JWT 并解析 claims。secret 是 HS256 对称密钥(字节原样传入;
// 调用方负责做 hex/ascii 等解码——该密钥是 ASCII 字符串,直接 []byte(secret) 即可,
// 具体长度/取值由运维通过环境变量注入,不在代码里硬编码)。
//
// 本函数是 bearer JWT 路径的唯一验签入口。通用 JWT 校验(alg/签名/过期/段数)走
// VerifyHS256JWT,这里只追加 本路径特有的 claims 约束(userId 必须存在且非零)。
// maxAge <= 0 表示不额外设上限,只用 token 自己的 exp(由 VerifyHS256JWT 强制)。
func verifyBearerJWT(token string, secret []byte, now time.Time, maxAge time.Duration) (*bearerJWTClaims, error) {
	var c bearerJWTClaims
	if err := VerifyHS256JWT(token, secret, now, &c); err != nil {
		return nil, err
	}
	if c.UserID <= 0 {
		return nil, ErrBearerJWTNoUserID
	}
	// iat 缺失必须拒绝,不能当作"很新"放行 —— 否则攻击者去掉 iat 就绕过下面的
	// 上限,那道检查就只是装饰。远期 iat 也要拒:它把 token 的可用窗口整体后移。
	//
	// 这两条与 maxAge 无关,两种用途都要。
	if c.Iat == 0 {
		return nil, ErrJWTMissingIat
	}
	issued := time.Unix(c.Iat, 0)
	if issued.After(now.Add(bearerJWTClockSkew)) {
		return nil, fmt.Errorf("%w: iat is %s ahead of now",
			ErrJWTIatInFuture, issued.Sub(now).Round(time.Second))
	}
	// maxAge 由调用方按用途给。**两个生产调用方现在都给 0**:
	//   - 兑换一次会话(/exchange-jwt):新鲜度由兑换台账判定(首次兑换上限 F +
	//     空闲窗口 T,见 redemption_ledger.go)。曾经这里给 10 分钟,锚点是 iat ——
	//     上游签发的时刻,与"用户什么时候真的来兑换"无关,于是登录半小时后才
	//     兑换的合法客户端被拒,而窗口内抓到 token 的攻击者照样能兑。
	//   - 常驻认证器(integration 端点):用 token 自己的 exp。桌面客户端把这张
	//     token 存下来长期复用、不重签,套上分钟级上限等于登录十分钟后功能永久失效。
	if maxAge > 0 && now.Sub(issued) > maxAge {
		return nil, fmt.Errorf("%w: issued %s ago, max %s",
			ErrJWTTooOld, now.Sub(issued).Round(time.Second), maxAge)
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

// issuerMaxLen issuer 允许的最大**字节**数,取 user_oidc_identity.issuer 的列宽
// 数值(VARCHAR(255))。
//
// 超长会在 INSERT 时被 MySQL 截断或报错,取决于 sql_mode —— 截断更危险:
// 两个不同 issuer 可能截成同一个值,uk_issuer_subject 就把两个人合成一个号。
// 所以在启动期拒绝,而不是等运行期。
//
// 与 subjectMaxLen 同一个数值、同一个"按字节比较故意比列宽更严"的取舍,理由见
// subject_shape.go 的注释。issuer 这侧几乎不受影响:它是绝对 URI,本就是 ASCII。
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
