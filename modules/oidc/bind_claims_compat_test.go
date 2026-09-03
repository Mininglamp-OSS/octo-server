package oidc

import "testing"

// 本次改动引入了 IdentityClaims 作为归一化身份的名字。它被刻意做成
// IDTokenClaims 的**类型别名**而不是新结构体,原因是 bind_store 在 Redis 里
// 存的是该结构体的 JSON 快照(encodeClaimsSnapshot / decodeClaimsSnapshot),
// 在途的自助绑定会话就靠它恢复 claims。改字段名或 JSON tag 会让**升级瞬间
// 已经存在于 Redis 中的绑定会话全部解不出来** —— 用户看到的是绑定流程莫名失败。
//
// 因此这里用一份**硬编码**的快照(模拟升级前写入的数据)来验证兼容性,而不是
// 序列化当前结构体再反序列化 —— 后者即使字段名改了也会通过,测不出这个风险。
func TestDecodeClaimsSnapshot_PreUpgradeSnapshotStillDecodes(t *testing.T) {
	// 这份 JSON 代表改动前 encodeClaimsSnapshot 的输出形态。
	legacy := []byte(`{
	  "iss": "https://idp.example.com",
	  "sub": "legacy-subject-1",
	  "email": "legacy@example.com",
	  "email_verified": true,
	  "phone_number": "13000000000",
	  "phone_number_verified": true,
	  "name": "Legacy User",
	  "nonce": "legacy-nonce",
	  "iat": 1700000000,
	  "exp": 1700003600,
	  "is_verified": true,
	  "verified_at": 1699999999,
	  "verified_provider": "cas",
	  "legal_name": "Legal Name",
	  "legal_email": "legal@example.com"
	}`)

	got, err := decodeClaimsSnapshot(legacy)
	if err != nil {
		t.Fatalf("a pre-upgrade snapshot no longer decodes; in-flight bind sessions would break: %v", err)
	}

	checks := []struct {
		field string
		got   interface{}
		want  interface{}
	}{
		{"Issuer", got.Issuer, "https://idp.example.com"},
		{"Subject", got.Subject, "legacy-subject-1"},
		{"Email", got.Email, "legacy@example.com"},
		{"EmailVerified", got.EmailVerified, true},
		{"PhoneNumber", got.PhoneNumber, "13000000000"},
		{"PhoneVerified", got.PhoneVerified, true},
		{"Name", got.Name, "Legacy User"},
		{"Nonce", got.Nonce, "legacy-nonce"},
		{"IssuedAt", got.IssuedAt, int64(1700000000)},
		{"Expiry", got.Expiry, int64(1700003600)},
		{"IsVerified", got.IsVerified.Bool(), true},
		{"VerifiedAt", got.VerifiedAt.Int64(), int64(1699999999)},
		{"VerifiedProvider", got.VerifiedProvider, "cas"},
		{"LegalName", got.LegalName, "Legal Name"},
		{"LegalEmail", got.LegalEmail, "legal@example.com"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v (JSON tag drift breaks in-flight bind sessions)", c.field, c.got, c.want)
		}
	}
}

// IdentityClaims 必须与 IDTokenClaims 是同一类型(别名),而不是"字段相同的两个
// 结构体"。若哪天有人把它改成独立结构体,下面这个赋值就编译不过 —— 那正是
// 我们想要的早期信号,因为 bind_store 的 wire 兼容性会在那一刻断掉。
func TestIdentityClaimsIsAliasOfIDTokenClaims(t *testing.T) {
	var a IDTokenClaims
	var b IdentityClaims = a // 别名成立时这行合法;若变成独立类型则编译失败
	if b.Subject != a.Subject {
		t.Fatal("unreachable")
	}
}
