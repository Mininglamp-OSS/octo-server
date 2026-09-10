package oidc

import "testing"

// (issuer, subject) 的比较必须逐字节精确。
//
// user_oidc_identity 的 COLLATE 是 utf8mb4_general_ci,而 uk_issuer_subject 建在
// (issuer, subject) 上。所以在数据库看来 "ABC" 与 "abc" 是**同一个 subject**:
//
//   - 查询 `WHERE subject='ABC'` 会命中一行 subject='abc' 的记录 → 两个不同的
//     上游主体落到同一个 uid,也就是账号接管;
//   - 而 checkSubjectShape 刻意放行含非数字字符的 subject(见 subject_shape.go),
//     所以一个字母数字混合的 subject 会真的走到这条路上。
//
// 修不改 collation 也不加 COLLATE 到 WHERE 里 —— 前者要在一张带唯一键的线上表
// 上重建索引,后者会让 subject 上的索引失效,而这是登录路径。
//
// 做法是在 Go 侧对返回行复核一次:不是逐字节相等就当作没查到。后果是安全的:
// 随后的 Insert 会被 ci 唯一键挡成 1062 → 走竞态恢复 → 再 Get 依然查不到 →
// 返回 nil → 登录被拒绝并留下审计。也就是把"静默合并两个人"换成"响亮地拒绝"。
func TestIdentityExactMatch_RejectsCaseFoldedRow(t *testing.T) {
	cases := []struct {
		name           string
		queried, found string
		wantMatch      bool
	}{
		{"identical", "abc123", "abc123", true},
		{"all digits (case folding is a no-op)", "823071756087671783", "823071756087671783", true},
		{"case differs", "ABC123", "abc123", false},
		{"one letter differs in case", "aBc123", "abc123", false},
		{"trailing space (ci collation also pads)", "abc123", "abc123 ", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := identitySubjectMatches(c.queried, c.found)
			if got != c.wantMatch {
				t.Errorf("identitySubjectMatches(queried=%q, found=%q) = %v, want %v",
					c.queried, c.found, got, c.wantMatch)
			}
		})
	}
}

// issuer 同样在唯一键里,同样会被折叠。
func TestIdentityExactMatch_ChecksIssuerToo(t *testing.T) {
	row := &IdentityModel{Issuer: "https://idp.example.com", Subject: "823071756087671783"}
	if !identityRowMatches(row, row.Issuer, row.Subject) {
		t.Fatal("an exact row must match itself")
	}
	if identityRowMatches(row, "https://IDP.example.com", row.Subject) {
		t.Error("a case-folded issuer was accepted; issuer is part of uk_issuer_subject " +
			"and folding it merges two identity namespaces")
	}
	if identityRowMatches(row, row.Issuer, "823071756087671784") {
		t.Error("a different subject was accepted")
	}
}

// nil 行照常表示"没有绑定",不能被复核逻辑变成 panic 或 false-positive。
func TestIdentityExactMatch_NilRowIsNotAMatch(t *testing.T) {
	if identityRowMatches(nil, "https://idp.example.com", "abc") {
		t.Error("a nil row must not match")
	}
}
