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

// checkSubjectShape 校验 subject 不是"短纯数字"这种危险形态。
//
// 只对纯数字生效:一旦含非数字字符,它就不可能是工号,长度也就不再是信号。
func checkSubjectShape(subject string) error {
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
