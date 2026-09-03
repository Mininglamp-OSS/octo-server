package oidc

// identity_exact_match.go — (issuer, subject) 的逐字节复核。
//
// 背景:user_oidc_identity 的 COLLATE 是 utf8mb4_general_ci,而 uk_issuer_subject
// 建在 (issuer, subject) 上。数据库因此认为 "ABC" 与 "abc" 是同一个 subject,
// 于是 `WHERE subject='ABC'` 会命中一行 subject='abc' 的记录 —— 两个不同的上游
// 主体落到同一个 uid,那是账号接管而不是脏数据。
//
// 为什么不在 SQL 层修:
//   - 改列的 collation 要在一张带唯一键的线上表上重建索引;
//   - 在 WHERE 里加 COLLATE 会让 subject 上的索引失效,而这是登录路径。
//
// 所以在 Go 侧复核一次。失效方向是安全的:复核不通过 → 当作"没有绑定" →
// 后续 Insert 被 ci 唯一键挡成 1062 → 走竞态恢复 → 再查依然不匹配 → 返回 nil
// → 登录被拒绝并留下审计行。也就是把"静默把两个人合成一个账号"换成
// "响亮地拒绝一次登录"。
//
// 今天这条路径在实践中不该被走到:文档给出的 subject 是 18 位数字,大小写折叠
// 对它是恒等变换。但 checkSubjectShape 刻意放行含非数字字符的 subject
// (它们不可能是工号),而真实 subject 的形态**至今未经实测确认** —— 所以这道
// 复核是为那个未知留的。

import (
	"errors"
	"strings"
)

// ErrIdentityCaseCollision 报告"ci 查询命中了一行,但它与查询值不是逐字节相等"。
//
// 为什么必须是一个**可区分的结果**,而不是复用"没查到"(nil, nil):
//
// 逐字节复核把"静默合并两个人"换成了"响亮拒绝",那个取舍是对的。但"没查到"会让
// ResolveOrLink 回 IsNew=true,而调用方拿到它就去 IssueSession(CreateUser=true) ——
// 用户行**先建出来**,随后的 identity Insert 才撞上 ci 唯一键报 1062,竞态恢复又
// 走同一个逐字节查询、同样看不见那行,于是返回 nil、登录被拒。
//
// 净效果:每一次登录尝试留下一个孤立 user 行,然后永久失败,且可无限重复
// (callback 只有全局 IP 底)。把它变成一个错误,拒绝就落在 ResolveOrLink 里,
// 在任何副作用之前。
var ErrIdentityCaseCollision = errors.New(
	"oidc: identity row differs only by case folding; refusing rather than merging identities")

// identitySubjectMatches 报告数据库返回的 subject 是否与查询用的 subject 逐字节相同。
//
// 用 strings.Compare 而非 == 只是为了让"这里刻意要求字节级相等"这件事在代码上
// 显式可见;两者行为一致。
func identitySubjectMatches(queried, found string) bool {
	return strings.Compare(queried, found) == 0
}

// identityRowMatches 报告一行 identity 是否精确对应查询用的 (issuer, subject)。
//
// issuer 也要查:它同样在唯一键里,同样会被折叠,而折叠 issuer 等于把两个身份
// 命名空间合并 —— 这正是 issuer 注入机制要防的事情。
func identityRowMatches(row *IdentityModel, issuer, subject string) bool {
	if row == nil {
		return false
	}
	return identitySubjectMatches(issuer, row.Issuer) &&
		identitySubjectMatches(subject, row.Subject)
}
