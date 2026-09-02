package oidc

import (
	"errors"
	"fmt"
	"strings"
)

// subject 形态守卫。
//
// 背景:`(issuer, subject)` 是 user_oidc_identity 的唯一键,也就是"这个人是谁"
// 的最终答案。它一旦落库就不可更改 —— 改了等于该 issuer 下全员按新账号重建。
//
// 而我们对这个 IdP 的 subject 到底是什么,拿到的文档是自相矛盾的:
//   - userinfo 的返回参数表把 `sub` 写作「子编号」,示例值是 18 位数字;
//   - 同一份文档的快速入门 Demo 里,注释却写「sub 获取用户工号」;
//   - 而工号在同一张表里另有其字段(`username`,注明"有工号优先工号"),
//     并且响应里还并存一个独立的 `user_id`。
//
// 三条线索合起来更支持「sub 是内部长 ID,不是工号」,但这只是推断。
// 真正要命的是推断错了的后果:**企业工号会被复用**。新入职者拿到离职者的工号,
// 就会命中离职者那条 identity 行,直接登进前任的账号 —— 跨人的信息泄露,
// 而且发现时数据已经污染,没有回滚路径。
//
// 所以这里做的不是"猜对形态",而是**把不可逆的污染换成可恢复的失败**:
// 形态不符就拒登并告警。拒登可以靠改配置/改代码恢复,串号不能。
//
// 判据刻意宽松,只排除"短纯数字"这一种明确危险的形态:
//   - 18 位数字(文档示例)→ 放行
//   - 20 位数字(ou_id 那种量级)→ 放行
//   - 含字母的标识(UUID、base64 之类)→ 放行,它们不可能是工号
//   - 7 位数字(观测到的工号量级)→ 拒绝
//
// 之所以不做成 env:这是安全基线而不是部署旋钮,放宽它应该留下一次 code review
// 的痕迹。真需要放宽时改这里的常量,同时把决策记进 task brief。
const (
	// minNumericSubjectLen 纯数字 subject 的最小可接受长度。
	//
	// 取 10:比观测到的工号长度(7 位)长,又远短于文档示例的
	// 18 位,留足余量以防对方的内部 ID 比示例短一些。
	minNumericSubjectLen = 10
)

// errSubjectTooShort subject 是纯数字且短到像工号。
var errSubjectTooShort = errors.New("subject looks like an employee number")

// errSubjectTooLong subject 超出 user_oidc_identity.subject 的列宽。
var errSubjectTooLong = errors.New("subject exceeds the identity column width")

// subjectMaxLen user_oidc_identity.subject 的列宽(VARCHAR(255),按字节)。
//
// 与 issuerMaxLen 同源同值:两列在同一个唯一键 uk_issuer_subject 里,分开维护
// 两个数字迟早不一致(有测试钉住它们相等)。
//
// 为什么必须在信任边界拒绝,而不是等 INSERT 报错:
//   - 严格 sql_mode 下 INSERT 会失败,但那已经是 IssueSession **建完用户之后**,
//     于是留下一个没有 identity 行的孤立用户,客户端只拿到 401;
//   - 非严格 sql_mode 下值被静默截断,前 255 字节相同的两个 subject 合成同一行 ——
//     账号接管,而且不可逆。
//
// issuer 那侧的注释把同一个论证写在 bearer_jwt.go;这里的论证更强,因为 issuer 是
// 运维配置的常量,而 subject 来自上游响应,长度不受我方控制。
const subjectMaxLen = issuerMaxLen

// checkSubjectShape 校验 subject 不是"短纯数字"这种危险形态。
//
// 只对纯数字生效:一旦含非数字字符,它就不可能是工号,长度也就不再是信号。
func checkSubjectShape(subject string) error {
	// 上限先查,且对所有形态生效 —— 下面的下限只管纯数字,放得过一个 300 位数字串。
	// 只回长度不回值:超长 subject 进日志既无用,又扩大 PII 面。
	if len(subject) > subjectMaxLen {
		return fmt.Errorf("%w: %d bytes, max %d (the column is VARCHAR(%d) inside "+
			"uk_issuer_subject; a longer value either fails the INSERT after the user "+
			"row already exists, or is silently truncated so that two subjects sharing "+
			"a prefix collapse onto one identity)",
			errSubjectTooLong, len(subject), subjectMaxLen, subjectMaxLen)
	}
	if !isAllDigits(subject) {
		return nil
	}
	if len(subject) >= minNumericSubjectLen {
		return nil
	}
	// 不回显 subject 本身 —— 它是用户标识。长度足够定位问题,
	// 而完整值会随错误信息进日志、也可能被回显给调用方。
	return fmt.Errorf("%w: %d-digit numeric subject is refused because employee "+
		"numbers are reused across joiners and leavers, which would link a new "+
		"hire to a former employee's account; expected an internal id of at least "+
		"%d digits", errSubjectTooShort, len(subject), minNumericSubjectLen)
}

// isAllDigits 空串返回 false(空 subject 由上游单独拒绝,不该走到这里)。
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) < 0
}
