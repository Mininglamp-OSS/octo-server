package oidc

// identity_bounds.go — (issuer, subject) 可落库性的**协议中立**守卫。
//
// 为什么需要一个协议中立的位置:
//
// 上限校验最初只挂在 plain-OAuth2 的信任边界上(parseUserInfoEnvelope 调
// checkSubjectShape)。那是当时唯一"响应体不可信"的路径,所以看起来够了。但列宽
// 是**存储**的性质,不是某个协议的性质:标准 OIDC 下一张签名合法、sub 300 字节的
// id_token 同样进不了 VARCHAR(255)。签名证明来源,不证明可存储。于是那条路上超长
// subject 一路走到 IssueSession 建号 + INSERT:
//   - 严格 sql_mode:INSERT 失败,留下没有 identity 行的孤立用户,客户端只见 401;
//   - 非严格 sql_mode:静默截断,前 255 字节相同的两个身份合成同一行 —— 账号接管,
//     而且 (issuer, subject) 落库后不可逆。
//
// 把守卫按 provider 各写一遍就又是一份会漂移的副本(这个 PR 已经在别处栽过:
// 一道守卫只挂在两个消费者之一上)。所以放在 claims **进入有状态路径**的那两个
// 入口:Service.ResolveOrLink 与 BindService.IssueWithReason。它们原本各自写了一份
// 一模一样的 "iss/sub 非空" 校验,合并之后重复反而少了一处。
//
// 这两个入口的位置也满足"副作用之前":ResolveOrLink 在 IssueSession 之前,而
// IssueWithReason 在 BindSession 落盘之前(Create 拿的是那份快照)。

import (
	"errors"
	"fmt"
	"strings"
)

// errIssuerTooLong issuer 超出 user_oidc_identity.issuer 的列宽。
var errIssuerTooLong = errors.New("issuer exceeds the identity column width")

// requireStorableIdentity 校验一组 claims 能作为 (issuer, subject) 主键落库。
//
// 三件事,顺序无关紧要但都必须做:
//  1. 两个字段非空 —— 空 subject 配 UNIQUE(issuer,subject) 会把所有空 sub 用户
//     塌成同一行,互相登进对方账号;
//  2. 两个字段不超列宽(见文件头);
//  3. subject 长度不超列宽(checkSubjectStorable)。
//
// **刻意不含**"短纯数字像工号"那条形态守卫。那条规则的论证是"工号被人事系统在
// 离职/入职之间复用",只对**上游 IdP 断言的** subject 成立;业务 JWT 那条路的
// subject 是我方业务库的主键(不复用),userId=42 是完全正常的取值。所以它留在
// 声明了 SubjectMayBeReusedPersonnelID 的 provider 里(目前只有 oauth2),按能力位
// 生效而非无条件 —— 由 AuthProvider 契约测试按 kind 分别钉住。见 checkUpstreamSubjectShape。
//
// 非空这条要显式写,不能指望长度检查:空串长度合法。
// identityEmailMaxLen / identityPhoneMaxLen 是 user_oidc_identity 另外两列的宽度。
//
// 守卫原先只盖 issuer/subject,而 inserter 写的是四列。超长的 email/phone 会重演
// 已经被修过两次的形态:IssueSession 先建号,之后 INSERT 失败(严格模式)或截断
// (非严格),下一次登录 autolink 到那个孤立账号上、再失败同一个 INSERT —— 永久卡住。
//
// phone 只有 32 字节,而 inserter 写的是**未归一化的原始** claims.PhoneNumber,
// 不是 normalizePhone 的产物 —— 所以一个带前缀和分隔符的上游值就能超。
const (
	identityEmailMaxLen = 255
	identityPhoneMaxLen = 32
)

var (
	errIdentityEmailTooLong = errors.New("email exceeds the identity column width")
	errIdentityPhoneTooLong = errors.New("phone exceeds the identity column width")
)

func requireStorableIdentity(claims *IDTokenClaims) error {
	if claims == nil {
		return errors.New("claims are nil")
	}
	issuer := strings.TrimSpace(claims.Issuer)
	subject := strings.TrimSpace(claims.Subject)
	if issuer == "" || subject == "" {
		return errors.New("claims iss/sub required")
	}
	if len(issuer) > issuerMaxLen {
		// 只回长度不回值 —— 与 subject 同样的理由,而 issuer 还可能含内部主机名。
		return fmt.Errorf("%w: %d bytes, max %d", errIssuerTooLong, len(issuer), issuerMaxLen)
	}
	// email / phone 也要界 —— 见上方常量的说明。只回长度不回值:两者都是 PII。
	if n := len(claims.Email); n > identityEmailMaxLen {
		return fmt.Errorf("%w: %d bytes, max %d", errIdentityEmailTooLong, n, identityEmailMaxLen)
	}
	if n := len(claims.PhoneNumber); n > identityPhoneMaxLen {
		return fmt.Errorf("%w: %d bytes, max %d", errIdentityPhoneTooLong, n, identityPhoneMaxLen)
	}
	return checkSubjectStorable(subject)
}
